package agent

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
	bolt "go.etcd.io/bbolt"
)

const logsBucket = "operation_logs"
const logEntriesBucket = "entries"

type localLogState struct {
	Next        uint64          `json:"next"`
	Ack         uint64          `json:"ack"`
	Bytes       uint64          `json:"bytes"`
	Limit       uint64          `json:"limit"`
	Truncated   bool            `json:"truncated"`
	Critical    map[string]bool `json:"critical"`
	SentThrough uint64          `json:"sent_through"`
	LastSentAt  time.Time       `json:"last_sent_at"`
}

type pendingLogChunk struct {
	OperationID uuid.UUID
	First, Last uint64
	JSON        []byte
}

func logKey(sequence uint64) []byte {
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, sequence)
	return key
}

func (s *State) appendOperationLog(id uuid.UUID, entry domain.BackupLogEntry) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if operations := tx.Bucket([]byte(operationsBucket)); operations == nil || operations.Get(id[:]) == nil {
			return errors.New("log operation is unavailable")
		}
		root, err := tx.CreateBucketIfNotExists([]byte(logsBucket))
		if err != nil {
			return err
		}
		bucket, err := root.CreateBucketIfNotExists(id[:])
		if err != nil {
			return err
		}
		entries, err := bucket.CreateBucketIfNotExists([]byte(logEntriesBucket))
		if err != nil {
			return err
		}
		var meta localLogState
		if raw := bucket.Get([]byte("meta")); raw != nil {
			if err := json.Unmarshal(raw, &meta); err != nil {
				return err
			}
		}
		if meta.Critical == nil {
			meta.Critical = make(map[string]bool)
		}
		entry.Sequence = meta.Next + 1
		entry.ObservedAt = entry.ObservedAt.UTC()
		if entry.Validate() != nil {
			return errors.New("invalid local log entry")
		}
		critical := entry.Code != "BACKUP_PROGRESS"
		key := entry.Section + ":" + entry.Code
		if critical && meta.Critical[key] {
			return nil
		}
		if meta.Limit == 0 {
			meta.Limit = uint64(domain.DefaultLogLimitBytes)
			var desired domain.DesiredState
			if active := tx.Bucket([]byte(desiredActiveBucket)); active != nil {
				if raw := active.Get([]byte(desiredStateKey)); raw != nil && json.Unmarshal(raw, &desired) == nil {
					meta.Limit = desired.RuntimePolicy.LogLimitBytes
				}
			}
			if meta.Limit < 64<<10 || meta.Limit > domain.DefaultLogLimitBytes {
				meta.Limit = domain.DefaultLogLimitBytes
			}
		}
		limit := meta.Limit
		payload, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		// Reserve room for one truncation marker and the bounded vocabulary of
		// error/Hook/summary/final events. Dropped progress consumes no sequence.
		if !critical && (meta.Truncated || meta.Bytes+uint64(len(payload)) > limit-(16<<10)) {
			if meta.Truncated {
				return nil
			}
			meta.Truncated = true
			entry.Code, entry.Level = "LOG_TRUNCATED", "WARN"
			payload, err = json.Marshal(entry)
			if err != nil {
				return err
			}
		}
		if meta.Bytes+uint64(len(payload)) > limit {
			return errors.New("local log limit exceeded")
		}
		if err := entries.Put(logKey(entry.Sequence), payload); err != nil {
			return err
		}
		meta.Next = entry.Sequence
		meta.Bytes += uint64(len(payload))
		if critical {
			meta.Critical[key] = true
		}
		raw, err := json.Marshal(meta)
		if err != nil {
			return err
		}
		return bucket.Put([]byte("meta"), raw)
	})
}

func (s *State) operationLog(id uuid.UUID, section, level, code string, files, bytes int64) error {
	return s.appendOperationLog(id, domain.BackupLogEntry{ObservedAt: time.Now().UTC(), Section: section, Level: level, Code: code, FilesDone: files, BytesDone: bytes})
}

func (s *State) pendingLogs(now time.Time, force bool) ([]pendingLogChunk, error) {
	chunks := make([]pendingLogChunk, 0, 8)
	err := s.db.View(func(tx *bolt.Tx) error {
		root := tx.Bucket([]byte(logsBucket))
		if root == nil {
			return nil
		}
		return root.ForEach(func(key, value []byte) error {
			if value != nil || len(key) != 16 || len(chunks) >= 8 {
				return nil
			}
			id, err := uuid.FromBytes(key)
			if err != nil {
				return err
			}
			var operation OperationRecord
			operations := tx.Bucket([]byte(operationsBucket))
			if operations == nil || json.Unmarshal(operations.Get(key), &operation) != nil {
				return errors.New("log operation missing")
			}
			if operation.Terminal() && !operation.Uploaded {
				return nil
			}
			if !operation.Terminal() && operation.LastReportedAt.IsZero() {
				return nil
			}
			bucket := root.Bucket(key)
			var meta localLogState
			if err := json.Unmarshal(bucket.Get([]byte("meta")), &meta); err != nil {
				return err
			}
			if meta.Ack >= meta.Next || !force && now.Sub(meta.LastSentAt) < resultRetryInterval {
				return nil
			}
			entries := bucket.Bucket([]byte(logEntriesBucket))
			if entries == nil {
				return nil
			}
			cursor := entries.Cursor()
			chunk := pendingLogChunk{OperationID: id, First: meta.Ack + 1, JSON: []byte{'['}}
			for k, v := cursor.Seek(logKey(meta.Ack + 1)); k != nil; k, v = cursor.Next() {
				if len(chunk.JSON)+len(v)+2 > domain.MaxBackupLogChunkBytes || chunk.Last >= chunk.First+199 {
					break
				}
				if chunk.Last != 0 {
					chunk.JSON = append(chunk.JSON, ',')
				}
				chunk.JSON = append(chunk.JSON, v...)
				chunk.Last = binary.BigEndian.Uint64(k)
			}
			if chunk.Last == 0 {
				return nil
			}
			chunk.JSON = append(chunk.JSON, ']')
			chunks = append(chunks, chunk)
			return nil
		})
	})
	return chunks, err
}

func (s *State) markLogsSent(id uuid.UUID, through uint64, now time.Time) error {
	return s.updateLogState(id, func(_ *bolt.Bucket, meta *localLogState) error {
		if through > meta.Next {
			return errors.New("invalid log send")
		}
		meta.SentThrough = through
		meta.LastSentAt = now
		return nil
	})
}

func (s *State) acknowledgeLogs(id uuid.UUID, through uint64) error {
	return s.updateLogState(id, func(bucket *bolt.Bucket, meta *localLogState) error {
		if through > meta.SentThrough || through > meta.Next {
			return errors.New("invalid log acknowledgement")
		}
		if through <= meta.Ack {
			return nil
		}
		entries := bucket.Bucket([]byte(logEntriesBucket))
		if entries != nil {
			cursor := entries.Cursor()
			for key, _ := cursor.First(); key != nil && binary.BigEndian.Uint64(key) <= through; key, _ = cursor.Next() {
				if err := cursor.Delete(); err != nil {
					return err
				}
			}
		}
		meta.Ack = through
		meta.LastSentAt = time.Time{}
		return nil
	})
}

func (s *State) updateLogState(id uuid.UUID, update func(*bolt.Bucket, *localLogState) error) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		root := tx.Bucket([]byte(logsBucket))
		if root == nil {
			return errors.New("unknown log operation")
		}
		bucket := root.Bucket(id[:])
		if bucket == nil {
			return errors.New("unknown log operation")
		}
		var meta localLogState
		if err := json.Unmarshal(bucket.Get([]byte("meta")), &meta); err != nil {
			return err
		}
		if err := update(bucket, &meta); err != nil {
			return err
		}
		raw, err := json.Marshal(meta)
		if err != nil {
			return err
		}
		return bucket.Put([]byte("meta"), raw)
	})
}
