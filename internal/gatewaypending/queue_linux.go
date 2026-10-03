// Package gatewaypending stores encrypted pending records, never authority.
package gatewaypending

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/security"
)

var ErrQueue = errors.New("gateway pending queue unavailable")

const reservedBytes int64 = 16 << 10

type Limits struct {
	MaxBytes   int64
	MaxRecords int
}

type identity struct {
	Binding      security.GatewayAuthorizationBinding
	Source       ed25519.PublicKey
	Recipient    [32]byte
	Confirmation ed25519.PublicKey
	Limits       Limits
}

// Queue has ONE owner. After restart Recover permits replay only. Neither
// reopening nor a successful drain permits resuming an old data-plane owner.
type Queue struct {
	mu                     sync.Mutex
	root                   *os.Root
	dir, lock              *os.File
	identity               identity
	key                    ed25519.PrivateKey
	files                  []string
	bytes                  int64
	sequence               int64
	hash                   string
	receipt                security.GatewayPendingReceipt
	failed, frozen, closed bool
	syncFile               func(*os.File) error // Fault injection; defaults to fsync.
}

func Create(path string, binding security.GatewayAuthorizationBinding, recipient [32]byte, source ed25519.PrivateKey, confirmation ed25519.PublicKey, limits Limits) (*Queue, error) {
	if len(source) != 64 {
		return nil, ErrQueue
	}
	public := source.Public().(ed25519.PublicKey)
	derived := ed25519.NewKeyFromSeed(source[:32])
	defer clear(derived)
	if !bytes.Equal(derived, source) {
		return nil, ErrQueue
	}
	return open(path, identity{binding, append(ed25519.PublicKey(nil), public...), recipient, append(ed25519.PublicKey(nil), confirmation...), limits}, source, true)
}

func Recover(path string, binding security.GatewayAuthorizationBinding, recipient [32]byte, source, confirmation ed25519.PublicKey, limits Limits) (*Queue, error) {
	return open(path, identity{binding, append(ed25519.PublicKey(nil), source...), recipient, append(ed25519.PublicKey(nil), confirmation...), limits}, nil, false)
}

func open(path string, id identity, key ed25519.PrivateKey, create bool) (*Queue, error) {
	if id.Binding.Validate() != nil || len(id.Source) != 32 || len(id.Confirmation) != 32 || id.Recipient == ([32]byte{}) ||
		id.Limits.MaxRecords < 1 || id.Limits.MaxRecords > 4096 || id.Limits.MaxBytes < security.MaxGatewayPendingSize+reservedBytes || id.Limits.MaxBytes > 64<<20 ||
		!filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, ErrQueue
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil || canonical != path {
		return nil, ErrQueue
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, ErrQueue
	}
	q := &Queue{root: root, identity: id, key: append(ed25519.PrivateKey(nil), key...), frozen: !create, hash: strings.Repeat("0", 64)}
	ok := false
	defer func() {
		if !ok {
			_ = q.Close()
		}
	}()
	q.dir, err = root.Open(".")
	if err != nil || !private(q.dir, true) {
		return nil, ErrQueue
	}
	q.lock, err = root.OpenFile("lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil || !private(q.lock, false) || syscall.Flock(int(q.lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return nil, ErrQueue
	}
	lockInfo, err := q.lock.Stat()
	if err != nil || lockInfo.Size() != 0 {
		return nil, ErrQueue
	}
	names, err := q.dir.Readdirnames(id.Limits.MaxRecords + 5)
	if err != nil && err != io.EOF {
		return nil, ErrQueue
	}
	if len(names) > id.Limits.MaxRecords+3 {
		return nil, ErrQueue
	}
	meta, err := json.Marshal(id)
	if err != nil {
		return nil, ErrQueue
	}
	if create {
		if len(names) != 1 || names[0] != "lock" || q.atomic("identity", meta) != nil {
			return nil, ErrQueue
		}
	} else {
		stored, err := q.read("identity", 4096)
		if err != nil || !bytes.Equal(meta, stored) {
			return nil, ErrQueue
		}
		if err = q.recover(names); err != nil {
			return nil, ErrQueue
		}
	}
	ok = true
	return q, nil
}

func private(f *os.File, directory bool) bool {
	if f == nil {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || sys.Uid != uint32(os.Geteuid()) {
		return false
	}
	if directory {
		return st.IsDir() && st.Mode().Perm() == 0700
	}
	return st.Mode().IsRegular() && st.Mode().Perm() == 0600 && sys.Nlink == 1
}

func (q *Queue) sync(f *os.File) error {
	if q.syncFile != nil {
		return q.syncFile(f)
	}
	return f.Sync()
}

func (q *Queue) atomic(name string, raw []byte) error {
	f, err := q.root.OpenFile("pending.tmp", os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return ErrQueue
	}
	n, err := f.Write(raw)
	if err == nil && n != len(raw) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = q.sync(f)
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return ErrQueue
	}
	if q.root.Rename("pending.tmp", name) != nil || q.sync(q.dir) != nil {
		return ErrQueue
	}
	return nil
}

func (q *Queue) read(name string, max int64) ([]byte, error) {
	f, err := q.root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, ErrQueue
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !private(f, false) || st.Size() > max {
		return nil, ErrQueue
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil || int64(len(b)) > max {
		return nil, ErrQueue
	}
	return b, nil
}

func (q *Queue) recover(names []string) error {
	if raw, err := q.read("receipt", 1024); err == nil {
		r, err := security.VerifyGatewayPendingReceipt(raw, q.identity.Confirmation)
		if err != nil || r.AdmissionID != q.identity.Binding.AdmissionID || r.RuntimeID != q.identity.Binding.RuntimeID {
			return ErrQueue
		}
		q.receipt = r
		q.sequence = r.Sequence
		q.hash = r.WireHash
	} else {
		// An absent receipt is the only valid initial state.
		if _, e := q.root.Lstat("receipt"); !errors.Is(e, os.ErrNotExist) {
			return ErrQueue
		}
	}
	sort.Strings(names)
	var acknowledged []string
	var physicalBytes int64
	for _, name := range names {
		info, err := q.root.Lstat(name)
		if err != nil || !info.Mode().IsRegular() {
			return ErrQueue
		}
		physicalBytes += info.Size()
		if physicalBytes > q.identity.Limits.MaxBytes {
			return ErrQueue
		}
	}
	for _, name := range names {
		if name == "identity" || name == "receipt" || name == "lock" {
			continue
		}
		if !strings.HasSuffix(name, ".record") || len(name) != 27 {
			return ErrQueue
		}
		seq, err := strconv.ParseInt(strings.TrimSuffix(name, ".record"), 10, 64)
		if err != nil || name != recordName(seq) {
			return ErrQueue
		}
		raw, err := q.read(name, security.MaxGatewayPendingSize)
		h, verifyErr := security.InspectGatewayPending(raw, q.identity.Source)
		if err != nil || verifyErr != nil || h.Binding != q.identity.Binding || h.Sequence != seq {
			return ErrQueue
		}
		hash := security.GatewayPendingHash(raw)
		if seq <= q.receipt.Sequence {
			if seq == q.receipt.Sequence && (h.RecordID != q.receipt.RecordID || hash != q.receipt.WireHash) {
				return ErrQueue
			}
			acknowledged = append(acknowledged, name)
			continue
		}
		if seq != q.sequence+1 || h.PreviousHash != q.hash {
			return ErrQueue
		}
		q.files = append(q.files, name)
		q.bytes += int64(len(raw))
		q.sequence = seq
		q.hash = hash
	}
	if len(q.files) > q.identity.Limits.MaxRecords || q.bytes+reservedBytes > q.identity.Limits.MaxBytes {
		return ErrQueue
	}
	// A durable verified receipt permits deletion even if the process crashed
	// between writing it and unlinking the already committed record.
	for _, name := range acknowledged {
		if q.root.Remove(name) != nil {
			return ErrQueue
		}
	}
	if len(acknowledged) > 0 && q.sync(q.dir) != nil {
		return ErrQueue
	}
	return nil
}

func recordName(sequence int64) string { return fmt.Sprintf("%020d.record", sequence) }

// Append fills queue-owned ordering and identity. A caller's success means
// durable acceptance only; it never acknowledges central replay or authorization.
func (q *Queue) Append(r security.GatewayPendingRecord) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.failed || q.frozen || len(q.files) >= q.identity.Limits.MaxRecords {
		return ErrQueue
	}
	if r.Header.Binding != (security.GatewayAuthorizationBinding{}) && r.Header.Binding != q.identity.Binding {
		return ErrQueue
	}
	id, err := uuid.NewV7()
	if err != nil {
		return ErrQueue
	}
	r.Header.Binding = q.identity.Binding
	r.Header.RecordID = id
	r.Header.Sequence = q.sequence + 1
	r.Header.PreviousHash = q.hash
	wire, err := security.SealGatewayPending(r, q.identity.Recipient, q.key)
	if err != nil || q.bytes+int64(len(wire))+reservedBytes > q.identity.Limits.MaxBytes {
		return ErrQueue
	}
	name := recordName(r.Header.Sequence)
	if q.atomic(name, wire) != nil {
		q.failed = true
		return ErrQueue
	}
	q.files = append(q.files, name)
	q.bytes += int64(len(wire))
	q.sequence = r.Header.Sequence
	q.hash = security.GatewayPendingHash(wire)
	return nil
}

func (q *Queue) Next() ([]byte, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.failed {
		return nil, ErrQueue
	}
	if len(q.files) == 0 {
		return nil, nil
	}
	wire, err := q.read(q.files[0], security.MaxGatewayPendingSize)
	h, e := security.InspectGatewayPending(wire, q.identity.Source)
	if err != nil || e != nil || h.Binding != q.identity.Binding || h.Sequence != q.receipt.Sequence+1 || h.PreviousHash != receiptHash(q.receipt) {
		q.failed = true
		return nil, ErrQueue
	}
	return wire, nil
}

func receiptHash(r security.GatewayPendingReceipt) string {
	if r.Sequence == 0 {
		return strings.Repeat("0", 64)
	}
	return r.WireHash
}

// Acknowledge persists the exact signed commit BEFORE reclaiming any space.
func (q *Queue) Acknowledge(wire []byte) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.failed {
		return ErrQueue
	}
	r, err := security.VerifyGatewayPendingReceipt(wire, q.identity.Confirmation)
	if err != nil || r.AdmissionID != q.identity.Binding.AdmissionID || r.RuntimeID != q.identity.Binding.RuntimeID {
		return ErrQueue
	}
	if r == q.receipt {
		return nil
	}
	if len(q.files) == 0 || r.Sequence != q.receipt.Sequence+1 {
		return ErrQueue
	}
	raw, err := q.read(q.files[0], security.MaxGatewayPendingSize)
	h, e := security.InspectGatewayPending(raw, q.identity.Source)
	if err != nil || e != nil || h.RecordID != r.RecordID || h.Sequence != r.Sequence || security.GatewayPendingHash(raw) != r.WireHash {
		return ErrQueue
	}
	if q.atomic("receipt", wire) != nil || q.root.Remove(q.files[0]) != nil || q.sync(q.dir) != nil {
		q.failed = true
		return ErrQueue
	}
	q.receipt = r
	q.bytes -= int64(len(raw))
	q.files = q.files[1:]
	return nil
}

// Freeze is called AFTER all appenders/data-plane work joined. It does not prove
// that happened. Tail lets the trusted coordinator seal the exact drained source.
func (q *Queue) Freeze() { q.mu.Lock(); defer q.mu.Unlock(); q.frozen = true }

func (q *Queue) Tail() (int64, string, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.failed || !q.frozen || len(q.files) != 0 {
		return 0, "", ErrQueue
	}
	return q.sequence, q.hash, nil
}

func (q *Queue) Close() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return nil
	}
	q.closed = true
	clear(q.key)
	var err error
	if q.lock != nil {
		err = errors.Join(err, q.lock.Close())
	}
	if q.dir != nil {
		err = errors.Join(err, q.dir.Close())
	}
	if q.root != nil {
		err = errors.Join(err, q.root.Close())
	}
	if err != nil {
		return ErrQueue
	}
	return nil
}
