package agent

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	bolt "go.etcd.io/bbolt"
)

const metadataBucket = "metadata"

type State struct {
	directory string
	db        *bolt.DB
}

func OpenState(directory string) (*State, error) {
	if directory == "" {
		return nil, errors.New("state directory is required")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, err
	}
	db, err := bolt.Open(filepath.Join(directory, "state.db"), 0o600, &bolt.Options{
		Timeout: time.Second,
	})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(filepath.Join(directory, "state.db"), 0o600); err != nil {
		_ = db.Close()
		return nil, err
	}
	state := &State{directory: directory, db: db}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(metadataBucket))
		return err
	}); err != nil {
		_ = db.Close()
		return nil, err
	}
	return state, nil
}

func (s *State) Close() error {
	return s.db.Close()
}

func (s *State) InstallID() (uuid.UUID, error) {
	var id uuid.UUID
	err := s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(metadataBucket))
		value := bucket.Get([]byte("install_id"))
		if value != nil {
			parsed, err := uuid.Parse(string(value))
			if err != nil {
				return errors.New("stored install identity is invalid")
			}
			id = parsed
			return nil
		}
		generated, err := uuid.NewV7()
		if err != nil {
			return err
		}
		if err := bucket.Put([]byte("install_id"), []byte(generated.String())); err != nil {
			return err
		}
		id = generated
		return nil
	})
	return id, err
}

func (s *State) Directory() string {
	return s.directory
}
