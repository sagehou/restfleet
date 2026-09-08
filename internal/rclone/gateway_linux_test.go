package rclone

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGatewayRuntimeRequiresBoundedLifetimeWithoutChangingShortCommands(t *testing.T) {
	r, err := NewRuntime(tmpfsRoot(t), fakeRclone(t, "success"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	persist := func(context.Context, []byte) error { return nil }
	called := false
	run := func(ctx context.Context, _, _ string) error {
		called = true
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) < 59*time.Minute {
			t.Error("gateway inherited short command timeout")
		}
		return nil
	}
	if err := r.WithGatewayConfig(context.Background(), []byte(testConfig()), "encrypted", persist, run); !errors.Is(err, ErrGatewayLifetime) || called {
		t.Fatal("unbounded gateway lifetime accepted")
	}
	long, stopLong := context.WithTimeout(context.Background(), 25*time.Hour)
	defer stopLong()
	if err := r.WithGatewayConfig(long, []byte(testConfig()), "encrypted", persist, run); !errors.Is(err, ErrGatewayLifetime) || called {
		t.Fatal("excessive gateway lifetime accepted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	if err := r.WithGatewayConfig(ctx, []byte(testConfig()), "encrypted", persist, run); err != nil || !called {
		t.Fatal(err)
	}
	if err := r.WithConfig(ctx, []byte(testConfig()), "encrypted", persist, func(ctx context.Context, _, _ string) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Minute {
			t.Error("short command timeout was relaxed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cancel()
	called = false
	if err := r.WithGatewayConfig(ctx, []byte(testConfig()), "encrypted", persist, run); !errors.Is(err, context.Canceled) || called {
		t.Fatal("canceled gateway runtime executed callback")
	}
}
