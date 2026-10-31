package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/candylario/trader/internal/testkit"
	"github.com/candylario/trader/internal/vault"
)

func TestHelpAndUnknownCommands(t *testing.T) {
	t.Chdir(t.TempDir())
	var out bytes.Buffer
	if err := run(context.Background(), nil, &out, env{}); err != nil || !strings.Contains(out.String(), "trader radar") {
		t.Fatalf("help: %v %s", err, out.String())
	}
	for _, args := range [][]string{{"decide"}, {"agent", "list"}, {"serve", "--config", "x"}, {"vault"}} {
		if err := run(context.Background(), args, &out, env{}); err == nil || !strings.Contains(err.Error(), "unknown command") {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestVaultRewrapAfterAKeyRotation(t *testing.T) {
	t.Chdir(t.TempDir())
	old, _ := vault.NewMasterKey()
	t.Setenv("DATABASE_URL", testkit.Database(t))
	t.Setenv("ENGINE_MASTER_KEY", old)
	t.Setenv("ENGINE_MASTER_KEY_PREVIOUS", "")
	ctx := context.Background()
	svc, closeAll, err := openFleet(ctx, env{})
	if err != nil {
		t.Fatal(err)
	}
	tn, _, err := svc.AddTenant(ctx, uuid.Nil, "bob")
	if err == nil {
		_, _, err = svc.Wallet(ctx, tn.ID)
	}
	closeAll()
	if err != nil {
		t.Fatal(err)
	}
	fresh, _ := vault.NewMasterKey()
	t.Setenv("ENGINE_MASTER_KEY", "2:"+fresh)
	t.Setenv("ENGINE_MASTER_KEY_PREVIOUS", old)
	var out bytes.Buffer
	if err := run(ctx, []string{"vault", "rewrap"}, &out, env{}); err != nil || !strings.Contains(out.String(), "re-wrapped 1 keys under master key version 2") {
		t.Fatalf("rewrap: %v %s", err, out.String())
	}
	t.Setenv("ENGINE_MASTER_KEY_PREVIOUS", "")
	out.Reset()
	if err := run(ctx, []string{"vault", "rewrap"}, &out, env{}); err != nil || !strings.Contains(out.String(), "re-wrapped 0 keys") {
		t.Fatalf("nothing left to rewrap: %v %s", err, out.String())
	}
	t.Setenv("ENGINE_MASTER_KEY", "nonsense")
	if err := run(ctx, []string{"vault", "rewrap"}, &out, env{}); err == nil || !strings.Contains(err.Error(), "ENGINE_MASTER_KEY") {
		t.Fatalf("a bad master key is refused by name: %v", err)
	}
}
