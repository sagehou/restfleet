package domain

import (
 "encoding/hex"
 "strings"

 "github.com/google/uuid"
)

// GatewayReadBinding is durable central-only scope, never an Agent capability.
type GatewayReadBinding struct {
 RepositoryID uuid.UUID `json:"repository_id"`
 OperationID uuid.UUID `json:"operation_id"`
 AdmissionID uuid.UUID `json:"admission_id"`
 RuntimeID uuid.UUID `json:"runtime_id"`
 ConfigurationHash string `json:"configuration_hash"`
}

func (b GatewayReadBinding) Valid() bool {
 for _,id:=range []uuid.UUID{b.RepositoryID,b.OperationID,b.AdmissionID,b.RuntimeID} {
  if id.Version()!=7 || id.Variant()!=uuid.RFC4122{return false}
 }
 _,err:=hex.DecodeString(b.ConfigurationHash)
 return err==nil && len(b.ConfigurationHash)==64 && strings.ToLower(b.ConfigurationHash)==b.ConfigurationHash
}
