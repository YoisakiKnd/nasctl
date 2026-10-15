package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/hubstore"
	"github.com/YoisakiKnd/nasctl/internal/operations"
)

func TestUserRolesAreEnforcedBeforeAnyHandlerRuns(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	called := false
	action := func(context.Context, *hubstore.Job, *operations.Args) (Result, error) {
		called = true
		return Result{Completed: true}, nil
	}
	viewer := domain.Principal{Kind: domain.PrincipalUser, Name: "reader", UserID: "reader-id", Role: domain.RoleViewer}
	out := r.m.Run(ctx, RunOptions{Principal: viewer, OpID: "files.mkdir", Args: operations.NewArgs().Str("root", "data").Str("path", "new"), Target: "data/new", OperatorConsent: true, Handler: action})
	if !errors.Is(out.Err, ErrDenied) || called || out.Decision.Gate != GateRole {
		t.Fatal("viewer mutation reached handler", out, called)
	}
	operator := domain.Principal{Kind: domain.PrincipalUser, Name: "operator", UserID: "operator-id", Role: domain.RoleOperator}
	out = r.m.Run(ctx, RunOptions{Principal: operator, OpID: "settings.configure", Args: operations.NewArgs().Str("revision", "revision"), Target: "next-start", OperatorConsent: true, Handler: action})
	if !errors.Is(out.Err, ErrDenied) || called || out.Decision.Gate != GateRole {
		t.Fatal("operator changed device configuration", out, called)
	}
	if e := r.m.StartMode(ctx, domain.ModeState{Mode: domain.ModeManualPause}, operator); !errors.Is(e, ErrDenied) {
		t.Fatal("operator changed global mode", e)
	}
	if e := r.m.Revoke(ctx, "anything", viewer); !errors.Is(e, ErrDenied) {
		t.Fatal("viewer revoked approval", e)
	}
}
func TestMetadataMaintenanceDuringPauseKeepsOtherGates(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	admin := domain.Principal{Kind: domain.PrincipalUser, Name: "owner", UserID: "owner-id", Role: domain.RoleAdmin}
	if e := r.m.StartMode(ctx, domain.ModeState{Mode: domain.ModeManualPause}, admin); e != nil {
		t.Fatal(e)
	}
	settingsOp, _ := r.m.Registry().Lookup("settings.configure")
	args := operations.NewArgs().Str("revision", "revision")
	if d := r.m.Admit(ctx, admin, settingsOp, args); !d.Allowed {
		t.Fatal("paused mode blocked bounded configuration maintenance", d)
	}
	viewer := domain.Principal{Kind: domain.PrincipalUser, Name: "reader", UserID: "reader-id", Role: domain.RoleViewer}
	if d := r.m.Admit(ctx, viewer, settingsOp, args); d.Gate != GateRole {
		t.Fatal("metadata label bypassed role gate", d)
	}
	r.m.cfg.ReadOnlyConsole = true
	if d := r.m.Admit(ctx, admin, settingsOp, args); d.Gate != GateReadOnly {
		t.Fatal("metadata label bypassed readonly gate", d)
	}
	forged := operations.Operation{ID: "files.copy", Summary: "forged", Class: domain.RiskMutating, Approval: domain.ApprovalPerCall, Timeout: time.Second, MetadataOnly: true}
	if e := forged.Validate(); e == nil {
		t.Fatal("file work forged metadata-only exception")
	}
}
