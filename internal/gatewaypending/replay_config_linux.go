package gatewaypending

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/sagehou/restfleet/internal/security"
)

var ErrReplayCommand = errors.New("gateway recovery replay unavailable or inconsistent")

// ReplayConfig is trusted local metadata for ONE existing encrypted queue.
// It contains only public keys, identifiers and paths, never secret material.
// It cannot register a source, append records, install an owner or release a fence.
type ReplayConfig struct {
	Version         int                                  `json:"version"`
	Binding         security.GatewayAuthorizationBinding `json:"binding,omitzero"`
	AuditOrigin     security.GatewayAuditBinding         `json:"audit_origin,omitzero"`
	SourcePublic    ed25519.PublicKey                    `json:"source_public"`
	RecipientPublic []byte                               `json:"recipient_public"`
	CentralPinFile  string                               `json:"central_pin_file"`
	QueueDirectory  string                               `json:"queue_directory"`
	MaxBytes        int64                                `json:"max_bytes"`
	MaxRecords      int                                  `json:"max_records"`
	SocketPath      string                               `json:"socket_path"`
	ServerUID       uint32                               `json:"server_uid"`
	SharedGroup     uint32                               `json:"shared_group"`
}

func (s ReplayConfig) Validate() error {
	global := s.AuditOrigin != (security.GatewayAuditBinding{})
	if s.Version != 1 || (!global && s.Binding.Validate() != nil) ||
		(global && (s.AuditOrigin.Validate() != nil || s.Binding != (security.GatewayAuthorizationBinding{}))) ||
		len(s.SourcePublic) != ed25519.PublicKeySize || len(s.RecipientPublic) != 32 || bytes.Equal(s.RecipientPublic, make([]byte, 32)) ||
		s.MaxBytes < security.MaxGatewayPendingSize+reservedBytes || s.MaxBytes > 64<<20 || s.MaxRecords < 1 || s.MaxRecords > 4096 ||
		s.ServerUID == ^uint32(0) || s.SharedGroup == ^uint32(0) || len(s.SocketPath) > 107 {
		return ErrReplayCommand
	}
	for _, path := range []string{s.CentralPinFile, s.QueueDirectory, s.SocketPath} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" || strings.IndexFunc(path, unicode.IsControl) >= 0 {
			return ErrReplayCommand
		}
	}
	uid := uint32(os.Geteuid())
	if (s.SharedGroup == 0 && s.ServerUID != uid) ||
		(s.SharedGroup != 0 && (uid == 0 || s.ServerUID == 0 || s.ServerUID == uid)) {
		return ErrReplayCommand
	}
	return nil
}

// LoadReplayConfig accepts the documented field order/encoding with whitespace.
// Exact re-encoding rejects duplicate/unknown/omitted/null/case-folded fields.
func LoadReplayConfig(path string) (ReplayConfig, error) {
	raw, err := security.ReadProtectedGatewayFile(path, 4096)
	if err != nil {
		return ReplayConfig{}, ErrReplayCommand
	}
	defer clear(raw)
	var s ReplayConfig
	if json.Unmarshal(raw, &s) != nil || s.Validate() != nil {
		return ReplayConfig{}, ErrReplayCommand
	}
	canonical, err := json.Marshal(s)
	var compact bytes.Buffer
	if err != nil || json.Compact(&compact, raw) != nil || !bytes.Equal(canonical, compact.Bytes()) {
		return ReplayConfig{}, ErrReplayCommand
	}
	return s, nil
}

// ReplayTail reports only an exactly acknowledged queue tail. Even after a
// successful recovery this is NOT proof that old processes/appenders stopped.
type ReplayTail struct {
	Version     int                                  `json:"version"`
	Binding     security.GatewayAuthorizationBinding `json:"binding,omitzero"`
	AuditOrigin security.GatewayAuditBinding         `json:"audit_origin,omitzero"`
	Sequence    int64                                `json:"sequence"`
	WireHash    string                               `json:"wire_hash"`
}

// ReplayFromConfig is one bounded recovery attempt using PUBLIC keys only.
// Recovery always freezes the producer, verifies the exact stored identity and
// retains uncertain evidence. No automatic retry, materialization or cleanup.
func ReplayFromConfig(ctx context.Context, s ReplayConfig) (tail ReplayTail, failure error) {
	if s.Validate() != nil || ctx.Err() != nil {
		return ReplayTail{}, ErrReplayCommand
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	central, err := security.LoadGatewayCentralPin(s.CentralPinFile)
	if err != nil {
		return ReplayTail{}, ErrReplayCommand
	}
	limits := Limits{MaxBytes: s.MaxBytes, MaxRecords: s.MaxRecords}
	recipient := [32]byte(s.RecipientPublic)
	runtime := s.Binding.RuntimeID
	var q *Queue
	if s.AuditOrigin != (security.GatewayAuditBinding{}) {
		runtime = s.AuditOrigin.RuntimeID
		q, err = RecoverGlobalAudit(s.QueueDirectory, s.AuditOrigin, recipient, s.SourcePublic, central, limits)
	} else {
		q, err = Recover(s.QueueDirectory, s.Binding, recipient, s.SourcePublic, central, limits)
	}
	if err != nil {
		return ReplayTail{}, ErrReplayCommand
	}
	defer func() {
		if q.Close() != nil {
			tail, failure = ReplayTail{}, ErrReplayCommand
		}
	}()
	if Drain(ctx, q, s.SocketPath, s.ServerUID, runtime, s.SharedGroup) != nil || ctx.Err() != nil {
		return ReplayTail{}, ErrReplayCommand
	}
	sequence, hash, err := q.Tail()
	if err != nil {
		return ReplayTail{}, ErrReplayCommand
	}
	return ReplayTail{Version: 1, Binding: s.Binding, AuditOrigin: s.AuditOrigin, Sequence: sequence, WireHash: hash}, nil
}
