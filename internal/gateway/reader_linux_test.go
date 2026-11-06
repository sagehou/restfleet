package gateway

import (
 "bytes"
 "context"
 "crypto/ed25519"
 "crypto/rand"
 "crypto/tls"
 "crypto/x509"
 "encoding/json"
 "encoding/pem"
 "io"
 "net/http"
 "net/http/httptest"
 "os"
 "path/filepath"
 "strings"
 "testing"
 "time"

 "github.com/google/uuid"
 "github.com/sagehou/restfleet/internal/domain"
 "github.com/sagehou/restfleet/internal/security"
)

func readerCertificate(t *testing.T,ca *security.AgentCA,b domain.GatewayReadBinding) tls.Certificate {
 t.Helper()
 cert,key,err:=ca.IssueGatewayReader(b,time.Now());if err!=nil{t.Fatal(err)};defer clear(key)
 pair,err:=tls.X509KeyPair(cert,key);if err!=nil{t.Fatal(err)}
 return pair
}

func TestGatewayReaderObjectsAreReadOnlyAndScopeBound(t *testing.T) {
 binding:=bindingFixture()
 reader:=domain.GatewayReadBinding{RepositoryID:binding.RepositoryID,OperationID:uuid.Must(uuid.NewV7()),AdmissionID:uuid.Must(uuid.NewV7()),RuntimeID:uuid.Must(uuid.NewV7()),ConfigurationHash:strings.Repeat("a",64)}
 ca,key,err:=security.NewAgentCA(time.Now());if err!=nil{t.Fatal(err)};defer clear(key)
 pair:=readerCertificate(t,ca,reader)
 backend:=&fakeBackend{objects:map[string]string{"config":"repository",object("locks","old-lock"):"old-lock"}}
 ctx,cancel:=context.WithTimeout(context.Background(),time.Minute);defer cancel()
 session,secret,err:=newBackupSession(ctx,binding,socketServer(t,backend),func(context.Context,Event)error{return nil},uuid.Must(uuid.NewV7()))
 if err!=nil{t.Fatal(err)};defer session.Close();clear(secret)
 session.reader=&reader
 request:=func(method,path string,leaf *x509.Certificate) int {
  t.Helper()
  req:=httptest.NewRequest(method,"https://gateway.invalid"+session.prefix+path,strings.NewReader("forbidden"))
  req.URL.Scheme,req.URL.Host="",""
  req.TLS=&tls.ConnectionState{Version:tls.VersionTLS13,PeerCertificates:[]*x509.Certificate{leaf},VerifiedChains:[][]*x509.Certificate{{leaf}}}
  response:=httptest.NewRecorder();session.ServeHTTP(response,req);return response.Code
 }
 for _,check:=range []struct{method,path string;status int}{
  {http.MethodGet,"config",200},{http.MethodHead,"config",200},{http.MethodGet,"snapshots/",200},
  {http.MethodPost,object("data","forbidden"),403},{http.MethodPost,object("locks","forbidden"),403},
  {http.MethodDelete,object("locks","old-lock"),403},{http.MethodDelete,"config",403},
 } {
  if got:=request(check.method,check.path,pair.Leaf);got!=check.status{t.Fatalf("%s %s = %d",check.method,check.path,got)}
 }
 for _,mutate:=range []func(*domain.GatewayReadBinding){
  func(b *domain.GatewayReadBinding){b.RepositoryID=uuid.Must(uuid.NewV7())},
  func(b *domain.GatewayReadBinding){b.OperationID=uuid.Must(uuid.NewV7())},
  func(b *domain.GatewayReadBinding){b.AdmissionID=uuid.Must(uuid.NewV7())},
  func(b *domain.GatewayReadBinding){b.RuntimeID=uuid.Must(uuid.NewV7())},
 } {
  other:=reader;mutate(&other)
  certificate:=readerCertificate(t,ca,other)
  if request(http.MethodGet,"config",certificate.Leaf)!=401{t.Fatal("reader scope crossed")}
 }
 observed:=backend.inspect()
 if len(observed.calls)!=3 || observed.objects[object("locks","old-lock")]!="old-lock"{t.Fatal("reader mutation reached backend")}
}

func TestServiceReaderMTLSRoleIsolationDeduplicationAndCleanup(t *testing.T) {
 f:=newServiceFixture(t,1,"success")
 ca,key,err:=security.NewAgentCA(time.Now());if err!=nil{t.Fatal(err)};defer clear(key)
 caPath:=filepath.Join(filepath.Dir(f.config.CertificateFile),"clients.pem")
 if err=os.WriteFile(caPath,ca.CertificatePEM(),0600);err!=nil{t.Fatal(err)}
 f.config.ClientCAFile=caPath
 s:=startServiceFixture(t,f)
 owner:=f.config.Repositories[0].Binding
 binding:=domain.GatewayReadBinding{RepositoryID:owner.RepositoryID,OperationID:uuid.Must(uuid.NewV7()),AdmissionID:owner.AdmissionID,RuntimeID:owner.RuntimeID,ConfigurationHash:owner.ConfigurationHash}
 clientFor:=func(pair tls.Certificate)*http.Client{
  t.Helper();client:=serviceTLSClient(t,f.certificate);client.Timeout=10*time.Second
  client.Transport.(*http.Transport).TLSClientConfig.Certificates=[]tls.Certificate{pair};return client
 }
 readerClient:=clientFor(readerCertificate(t,ca,binding))
 _,agentKey,err:=ed25519.GenerateKey(rand.Reader);if err!=nil{t.Fatal(err)};defer clear(agentKey)
 csr,err:=x509.CreateCertificateRequest(rand.Reader,&x509.CertificateRequest{},agentKey);if err!=nil{t.Fatal(err)}
 issued,err:=ca.IssueAgentCertificate(pem.EncodeToMemory(&pem.Block{Type:"CERTIFICATE REQUEST",Bytes:csr}),owner.AgentID,time.Now());if err!=nil{t.Fatal(err)}
 rawKey,err:=x509.MarshalPKCS8PrivateKey(agentKey);if err!=nil{t.Fatal(err)};defer clear(rawKey)
 keyPEM:=pem.EncodeToMemory(&pem.Block{Type:"PRIVATE KEY",Bytes:rawKey});defer clear(keyPEM)
 pair,err:=tls.X509KeyPair(issued.CertificatePEM,keyPEM);if err!=nil{t.Fatal(err)}
 agentClient:=clientFor(pair)
 root:="https://"+s.listener.Addr().String()
 route:="/gateway/v1/repositories/"+binding.RepositoryID.String()+"/read-sessions/"+binding.OperationID.String()
 deadline:=time.Now().UTC().Add(time.Minute)
 body,_:=json.Marshal(struct{Deadline time.Time `json:"deadline"`}{deadline})
 request:=func(client *http.Client,method,path string,body []byte)(int,[]byte) {
  t.Helper();req,err:=http.NewRequest(method,root+path,bytes.NewReader(body));if err!=nil{t.Fatal(err)}
  if method==http.MethodPost{req.Header.Set("Content-Type","application/json")}
  response,err:=client.Do(req);if err!=nil{t.Fatal(err)};defer response.Body.Close()
  raw,err:=io.ReadAll(response.Body);if err!=nil{t.Fatal(err)};return response.StatusCode,raw
 }
 if code,_:=request(agentClient,http.MethodPost,route,body);code!=403{t.Fatal("Agent acquired reader session")}
 if code,_:=request(readerClient,http.MethodPost,strings.Replace(route,"read-sessions","backup-sessions",1),body);code!=403{t.Fatal("reader acquired backup session")}
 for _,mutate:=range []func(*domain.GatewayReadBinding){
  func(b *domain.GatewayReadBinding){b.RuntimeID=uuid.Must(uuid.NewV7())},
  func(b *domain.GatewayReadBinding){b.AdmissionID=uuid.Must(uuid.NewV7())},
  func(b *domain.GatewayReadBinding){b.RepositoryID=uuid.Must(uuid.NewV7())},
 } {
  other:=binding;mutate(&other)
  if code,_:=request(clientFor(readerCertificate(t,ca,other)),http.MethodPost,route,body);code!=403{t.Fatal("stale or cross-repository reader admitted")}
 }
 var session serviceBackupResponse
 until:=time.Now().Add(5*time.Second)
 for {
  code,raw:=request(readerClient,http.MethodPost,route,body)
  if code==409 && time.Now().Before(until){time.Sleep(10*time.Millisecond);continue}
  if (code!=200 && code!=202) || json.Unmarshal(raw,&session)!=nil || session.State!="ACTIVE"{t.Fatalf("reader session = %d %s",code,raw)}
  break
 }
 if session.ExpiresAt.After(deadline){t.Fatal("read exceeded requested deadline")}
 code,raw:=request(readerClient,http.MethodPost,route,body)
 var replay serviceBackupResponse
 if code!=200 || json.Unmarshal(raw,&replay)!=nil || replay.SessionID!=session.SessionID || !replay.ExpiresAt.Equal(session.ExpiresAt){t.Fatal("duplicate reader opened another session")}
 if code,_=request(agentClient,http.MethodGet,session.EndpointPath+"config",nil);code!=401{t.Fatal("Agent read central session")}
 if code,_=request(readerClient,http.MethodGet,session.EndpointPath+"config",nil);code!=200{t.Fatal("reader could not read")}
 if code,_=request(readerClient,http.MethodPost,session.EndpointPath+object("locks","forbidden"),[]byte("forbidden"));code!=403{t.Fatal("reader created object")}
 for range 2 {if code,_=request(readerClient,http.MethodDelete,route,nil);code!=204{t.Fatal("reader cleanup not idempotent")}}
 if code,_=request(readerClient,http.MethodGet,session.EndpointPath+"config",nil);code!=403{t.Fatal("closed reader path survived")}
 // Closing this session keeps the existing material owner usable.
 if !s.repositories[0].owner.ready(){t.Fatal("ordinary read cleanup poisoned owner")}
 next:=startServiceBackup(t,s,binding.RepositoryID);_=waitAccess(t,next);finishSupervised(t,next)
}
