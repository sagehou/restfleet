package restic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/rclone"
)

// ErrInvalidSnapshotIndex rejects malformed central snapshot-index requests.
var ErrInvalidSnapshotIndex = errors.New("invalid snapshot index request")

const (
	maxIndexSnapshots = 50_000
	maxIndexTags      = 100
	maxIndexPaths     = 1_000
)

// SnapshotIndexRequest is borrowed central-only repository material.
type SnapshotIndexRequest struct {
	Config       []byte
	Remote       string
	GatewayID    uuid.UUID
	RepositoryID uuid.UUID
	Password     []byte
}

func (r *SnapshotIndexRequest) normalize() error {
	if r.GatewayID.Version() != 7 || r.GatewayID.Variant() != uuid.RFC4122 ||
		r.RepositoryID.Version() != 7 || r.RepositoryID.Variant() != uuid.RFC4122 ||
		len(r.Password) < 32 || len(r.Password) > 1024 ||
		bytes.ContainsAny(r.Password, "\x00\r\n") {
		return ErrInvalidSnapshotIndex
	}
	return nil
}

// SnapshotIndex runs one read-only `restic snapshots --json` through the
// central repository scope. The caller resolves RestFleet tags into Plan IDs.
func (p *Provisioner) SnapshotIndex(
	ctx context.Context,
	request SnapshotIndexRequest,
	persist func(context.Context, []byte) error,
) (domain.SnapshotIndexResult, error) {
	if err := request.normalize(); err != nil {
		return domain.SnapshotIndexResult{}, err
	}
	var result domain.SnapshotIndexResult
	err := p.runtime.WithConfig(ctx, request.Config, request.Remote, persist,
		func(ctx context.Context, config, binary string) error {
			return p.withRepository(ctx, config, binary, request.Remote, request.GatewayID, request.RepositoryID, request.Password,
				func(workCtx context.Context, dir, repository, passwordFile string) error {
					raw, err := p.run(workCtx, dir, repository, passwordFile, "snapshots")
					if err != nil {
						return err
					}
					parsed, err := parseSnapshotIndex(raw)
					if err != nil {
						return err
					}
					result = parsed
					return nil
				})
		})
	if errors.Is(err, rclone.ErrCommandFailed) {
		return domain.SnapshotIndexResult{}, err
	}
	if err != nil {
		return domain.SnapshotIndexResult{}, err
	}
	return result, nil
}

func parseSnapshotIndex(raw []byte) (domain.SnapshotIndexResult, error) {
	var wire []struct {
		ID             string   `json:"id"`
		Tree           string   `json:"tree"`
		Time           string   `json:"time"`
		Paths          []string `json:"paths"`
		Tags           []string `json:"tags"`
		ProgramVersion string   `json:"program_version"`
		Summary        *struct {
			FilesNew  int64 `json:"files_new"`
			DataAdded int64 `json:"data_added"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil || len(wire) > maxIndexSnapshots {
		return domain.SnapshotIndexResult{}, ErrInvalidOutput
	}
	result := domain.SnapshotIndexResult{Entries: make([]domain.IndexedSnapshot, 0, len(wire))}
	for _, item := range wire {
		if !repositoryIDPattern.MatchString(item.ID) || !repositoryIDPattern.MatchString(item.Tree) ||
			len(item.ProgramVersion) > 128 || len(item.Tags) > maxIndexTags || len(item.Paths) > maxIndexPaths {
			return domain.SnapshotIndexResult{}, ErrInvalidOutput
		}
		captured, err := time.Parse(time.RFC3339Nano, item.Time)
		if err != nil {
			return domain.SnapshotIndexResult{}, ErrInvalidOutput
		}
		for _, tag := range item.Tags {
			if tag == "" || len(tag) > 256 || strings.IndexFunc(tag, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
				return domain.SnapshotIndexResult{}, ErrInvalidOutput
			}
		}
		for _, path := range item.Paths {
			if len(path) == 0 || len(path) > 4096 || !strings.HasPrefix(path, "/") ||
				strings.IndexFunc(path, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
				return domain.SnapshotIndexResult{}, ErrInvalidOutput
			}
		}
		entry := domain.IndexedSnapshot{
			SnapshotID: item.ID, TreeID: item.Tree, CapturedAt: captured.UTC(),
			Tags: append([]string(nil), item.Tags...), Paths: append([]string(nil), item.Paths...),
			ProgramVersion: item.ProgramVersion,
		}
		if item.Summary != nil {
			entry.FilesNew, entry.BytesAdded = item.Summary.FilesNew, item.Summary.DataAdded
		}
		result.Entries = append(result.Entries, entry)
	}
	return result, nil
}
