package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestHookAllowlistRejectsUnsafeFilesAndStopsProcessGroup(t *testing.T) {
	directory:=t.TempDir()
	if err:=os.Chmod(directory,0700);err!=nil{t.Fatal(err)}
	executable:=filepath.Join(directory,"hook")
	if err:=os.WriteFile(executable,[]byte("#!/bin/sh\nprintf 'secret-output-canary' >&2\nexit 0\n"),0700);err!=nil{t.Fatal(err)}
	id:=uuid.Must(uuid.NewV7())
	path:=filepath.Join(directory,"hooks.json")
	raw,err:=json.Marshal(map[string]any{"version":1,"hooks":[]LocalHook{{ID:id,Executable:executable,TimeoutSeconds:1}}})
	if err!=nil{t.Fatal(err)}
	if err:=os.WriteFile(path,raw,0600);err!=nil{t.Fatal(err)}
	hooks,err:=LoadHooks(path)
	if err!=nil || len(hooks)!=1{t.Fatalf("allowlist rejected: %v",err)}
	state:=&State{hooks:hooks}
	if err:=state.runHooks(context.Background(),[]uuid.UUID{id});err!=nil{t.Fatal(err)}
	if err:=state.runHooks(context.Background(),[]uuid.UUID{uuid.Must(uuid.NewV7())});err==nil{t.Fatal("unknown Hook ran")}
	if err:=os.Chmod(path,0644);err!=nil{t.Fatal(err)}
	if _,err:=LoadHooks(path);err==nil{t.Fatal("public allowlist accepted")}
	if err:=os.Chmod(path,0600);err!=nil{t.Fatal(err)}
	if err:=os.Chmod(executable,0777);err!=nil{t.Fatal(err)}
	if _,err:=LoadHooks(path);err==nil{t.Fatal("world writable Hook accepted")}
	if err:=os.Chmod(executable,0700);err!=nil{t.Fatal(err)}
	link:=filepath.Join(directory,"link.json")
	if err:=os.Symlink(path,link);err!=nil{t.Fatal(err)}
	if _,err:=LoadHooks(link);err==nil{t.Fatal("symlink allowlist accepted")}
	if err:=os.WriteFile(executable,[]byte("#!/bin/sh\nprintf 'secret-output-canary' >&2\nsleep 30 &\nwait\n"),0700);err!=nil{t.Fatal(err)}
	started:=time.Now()
	err=state.runHooks(context.Background(),[]uuid.UUID{id})
	if err==nil || strings.Contains(err.Error(),"canary") || time.Since(started)>5*time.Second{t.Fatalf("Hook timeout/redaction failed: %v",err)}
}
