// Package schedule computes deterministic local run times for one plan.
//
// It owns the Agent-side half of SCH-001–006: five-field cron in the plan's
// IANA timezone, stable jitter derived from (plan_id, scheduled_at), the
// misfire grace decision and the FORBID concurrency rule. It deliberately does
// not execute backups; the local loop that owns desired state calls it.
package schedule

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// maxMinutesAhead bounds the search to one leap year of minutes.
const maxMinutesAhead = 366 * 24 * 60

var fieldBounds = [5]struct{ min, max int }{
	{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6},
}

// Spec is the subset of plan config the scheduler needs.
type Spec struct {
	Cron                string
	Timezone            string
	JitterSeconds       int
	MisfireGraceSeconds int
	// ConcurrencyPolicy follows the plan contract; only FORBID changes behaviour.
	ConcurrencyPolicy string
}

// Scheduler is immutable once built and safe for concurrent use.
type Scheduler struct {
	planID        uuid.UUID
	spec          Spec
	loc           *time.Location
	fields        [5]uint64
	domRestricted bool
	dowRestricted bool
}

// New parses the spec once. Invalid input is rejected instead of silently
// falling back to a default schedule.
func New(planID uuid.UUID, spec Spec) (*Scheduler, error) {
	loc, err := time.LoadLocation(spec.Timezone)
	if err != nil || spec.Timezone == "" || spec.Timezone == "Local" {
		return nil, errors.New("schedule: invalid timezone")
	}
	parts := strings.Fields(spec.Cron)
	if len(parts) != len(fieldBounds) {
		return nil, errors.New("schedule: cron must have five fields")
	}
	s := &Scheduler{planID: planID, spec: spec, loc: loc}
	for i, part := range parts {
		mask, wildcard, err := parseField(part, fieldBounds[i].min, fieldBounds[i].max)
		if err != nil {
			return nil, err
		}
		s.fields[i] = mask
		if i == 2 {
			s.domRestricted = !wildcard
		}
		if i == 4 {
			s.dowRestricted = !wildcard
		}
	}
	if spec.JitterSeconds < 0 || spec.JitterSeconds > 3600 {
		return nil, errors.New("schedule: jitter out of range")
	}
	if spec.MisfireGraceSeconds < 0 || spec.MisfireGraceSeconds > 86400 {
		return nil, errors.New("schedule: misfire grace out of range")
	}
	return s, nil
}

// NextScheduled returns the next scheduled minute strictly after t.
func (s *Scheduler) NextScheduled(t time.Time) (time.Time, bool) {
	start := t.Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < maxMinutesAhead; i++ {
		candidate := start.Add(time.Duration(i) * time.Minute)
		if s.matches(candidate.In(s.loc)) {
			return candidate, true
		}
	}
	return time.Time{}, false
}

// NextRun applies the stable jitter on top of the scheduled minute.
func (s *Scheduler) NextRun(t time.Time) (time.Time, bool) {
	scheduled, ok := s.NextScheduled(t)
	if !ok {
		return time.Time{}, false
	}
	return scheduled.Add(StableJitter(s.planID, scheduled, s.spec.JitterSeconds)), true
}

func (s *Scheduler) matches(local time.Time) bool {
	if !bit(s.fields[0], local.Minute(), 0) || !bit(s.fields[1], local.Hour(), 0) ||
		!bit(s.fields[3], int(local.Month()), 1) {
		return false
	}
	domMatch := bit(s.fields[2], local.Day(), 1)
	dowMatch := bit(s.fields[4], int(local.Weekday()), 0)
	// POSIX: when both day fields are restricted, either may match.
	if s.domRestricted && s.dowRestricted {
		return domMatch || dowMatch
	}
	return domMatch && dowMatch
}

func bit(mask uint64, value, offset int) bool {
	if value < offset {
		return false
	}
	return mask&(1<<uint(value-offset)) != 0
}

// StableJitter is derived from the plan and the scheduled instant, so restarts
// and different Agents produce the same delay.
func StableJitter(planID uuid.UUID, scheduled time.Time, maxSeconds int) time.Duration {
	if maxSeconds <= 0 {
		return 0
	}
	sum := sha256.Sum256([]byte(planID.String() + "|" + scheduled.UTC().Format(time.RFC3339)))
	value := binary.BigEndian.Uint64(sum[:8])
	return time.Duration(value%uint64(maxSeconds+1)) * time.Second
}

// MisfireDecision is what the local loop should do with a missed run.
type MisfireDecision string

const (
	RunCompensation MisfireDecision = "RUN_ONCE"
	ReportMissed    MisfireDecision = "MISSED"
)

// DecideMisfire allows one compensating run inside the grace window and none
// after it. The caller records the scheduled time so a restart cannot replay it.
func DecideMisfire(now, scheduled time.Time, graceSeconds int) MisfireDecision {
	if !now.After(scheduled) {
		return RunCompensation
	}
	if now.Sub(scheduled) <= time.Duration(graceSeconds)*time.Second {
		return RunCompensation
	}
	return ReportMissed
}

// ConcurrencySkip reports whether a new run must be skipped because the
// previous one is still running under FORBID.
func ConcurrencySkip(policy string, running bool) bool {
	return strings.EqualFold(policy, "FORBID") && running
}

// OperationID returns the deterministic local operation key for one scheduled
// time, so retries and restarts cannot create a second run.
func OperationID(planID uuid.UUID, scheduled time.Time) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(planID.String()+"|"+scheduled.UTC().Format(time.RFC3339)))
}

func parseField(part string, min, max int) (uint64, bool, error) {
	var mask uint64
	wildcard := part == "*"
	for _, item := range strings.Split(part, ",") {
		if item == "" {
			return 0, false, errors.New("schedule: empty list item")
		}
		step := 1
		if base, rawStep, ok := strings.Cut(item, "/"); ok {
			parsed, err := strconv.Atoi(rawStep)
			if err != nil || parsed < 1 {
				return 0, false, errors.New("schedule: invalid step")
			}
			step = parsed
			item = base
		}
		low, high := min, max
		switch {
		case item == "*":
		case strings.Contains(item, "-"):
			left, right, _ := strings.Cut(item, "-")
			first, err := strconv.Atoi(left)
			if err != nil {
				return 0, false, errors.New("schedule: invalid range")
			}
			last, err := strconv.Atoi(right)
			if err != nil || first < min || last > max || first > last {
				return 0, false, errors.New("schedule: range out of bounds")
			}
			low, high = first, last
		default:
			value, err := strconv.Atoi(item)
			if err != nil || value < min || value > max {
				return 0, false, errors.New("schedule: value out of bounds")
			}
			low, high = value, value
		}
		for value := low; value <= high; value += step {
			mask |= 1 << uint(value-min)
		}
	}
	return mask, wildcard, nil
}
