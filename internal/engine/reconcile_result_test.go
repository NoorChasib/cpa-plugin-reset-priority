package engine

import (
	"context"
	"testing"
)

func TestReconcileReportsSuccessfulPass(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.addAccount("claude", "a", day(1))

	if got := env.eng.Reconcile(context.Background()); got != ReconcileResultSuccess {
		t.Fatalf("reconcile result = %q, want %q", got, ReconcileResultSuccess)
	}
}

func TestReconcileReportsNoOpWhenStopped(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.eng.Stop()

	if got := env.eng.Reconcile(context.Background()); got != ReconcileResultNoOp {
		t.Fatalf("reconcile result = %q, want %q", got, ReconcileResultNoOp)
	}
}

func TestReconcileReportsErrorWhenRosterCannotBeRead(t *testing.T) {
	env := newTestEnv(t, defaultConfig())
	env.host.listErr = errFake("roster unavailable")

	if got := env.eng.Reconcile(context.Background()); got != ReconcileResultError {
		t.Fatalf("reconcile result = %q, want %q", got, ReconcileResultError)
	}
}
