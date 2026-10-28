package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Regression tests for docs/spec final audit findings (#2 #3 #4). The risk
// cases (#1) are in risk_test.go and the terminal cases (#5 #6) in
// terminal_test.go.

// #2: the keychain error keeps the stop cause, so classifyExecError says
// canceled, not credential_unavailable.
func TestUnavailableKeepsStopCause(t *testing.T) {
	for _, cause := range []error{errDisconnected, errShuttingDown, errClientGone} {
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(cause)
		err := newCredentialError(unavailable(ctx))
		if !errors.Is(err, ErrSecretUnavailable) || !errors.Is(err, cause) {
			t.Fatalf("%v: errors.Is lost a cause: %v", cause, err)
		}
		if kind := classifyExecError(err); kind != "canceled" {
			t.Fatalf("%v: kind=%s, want canceled", cause, kind)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	if kind := classifyExecError(newCredentialError(unavailable(ctx))); kind != "credential_unavailable" {
		t.Fatalf("keychain timeout kind=%s, want credential_unavailable", kind)
	}
}

// #2: a disconnect while the approval waits for the keychain is a cancel.
func TestApproveDisconnectDuringKeychainWait(t *testing.T) {
	app := newTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTrustedConnection(t, app, srv.Connection("lab"))
	fault := swapSecrets(app)
	fault.hang.Store(true)
	req := queueOn(t, app, conn, "rm -rf /tmp/x", "agent")

	done := make(chan ActionResult, 1)
	go func() { done <- app.ApproveCommand(req.ID, "") }()
	sessWaitFor(t, 3*time.Second, "keychain get", func() bool { return fault.count("get") > 0 })
	if r := app.DisconnectConnection(conn.ID); !r.OK {
		t.Fatalf("disconnect: %+v", r)
	}
	var result ActionResult
	select {
	case result = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("approve did not return after disconnect")
	}
	if result.OK || result.Code != "canceled" {
		t.Fatalf("approve: %+v, want canceled", result)
	}
	got, _ := app.findRequest(req.ID)
	if got.Status != stateFailed || got.ErrorKind != "canceled" {
		t.Fatalf("status=%s kind=%s, want failed/canceled", got.Status, got.ErrorKind)
	}
	if len(srv.Execs()) != 0 {
		t.Fatalf("command ran: %v", srv.Execs())
	}
}

// #2 / §5.0: shutdown while the approval waits for the keychain is
// recorded as interrupted.
func TestShutdownDuringKeychainWaitInterrupts(t *testing.T) {
	old := shutdownExecGrace
	shutdownExecGrace = 200 * time.Millisecond
	t.Cleanup(func() { shutdownExecGrace = old })

	app, _ := newSessTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTrustedConnection(t, app, srv.Connection("lab"))
	fault := swapSecrets(app)
	fault.hang.Store(true)
	req := queueOn(t, app, conn, "rm -rf /tmp/x", "agent")

	done := make(chan ActionResult, 1)
	go func() { done <- app.ApproveCommand(req.ID, "") }()
	sessWaitFor(t, 3*time.Second, "keychain get", func() bool { return fault.count("get") > 0 })
	app.shutdown(context.Background())
	var result ActionResult
	select {
	case result = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("approve did not return after shutdown")
	}
	if result.OK || result.Code != "shutting_down" {
		t.Fatalf("approve: %+v, want shutting_down", result)
	}
	got, _ := app.findRequest(req.ID)
	if got.Status != stateInterrupted || got.ErrorKind != "canceled" {
		t.Fatalf("status=%s kind=%s, want interrupted/canceled", got.Status, got.ErrorKind)
	}
}

// #3: a storage failure during reject is state_storage, not not_found, and
// the request stays pending.
func TestRejectStorageFailureIsNotNotFound(t *testing.T) {
	app := newTestApp(t)
	srv := newSSHTestServer(t)
	conn := addTrustedConnection(t, app, srv.Connection("lab"))
	req := queueOn(t, app, conn, "rm -rf /tmp/x", "agent")
	if _, err := app.store.db.Exec(`CREATE TRIGGER fail_req_update BEFORE UPDATE ON command_requests BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
		t.Fatal(err)
	}
	r := app.RejectCommand(req.ID)
	if r.OK || r.Code != "state_storage" || !strings.Contains(r.Message, "disk I/O error") {
		t.Fatalf("reject with storage failure: %+v, want state_storage", r)
	}
	if got, _ := app.findRequest(req.ID); got.Status != statePending {
		t.Fatalf("status=%s, want pending", got.Status)
	}
	if _, err := app.store.db.Exec(`DROP TRIGGER fail_req_update`); err != nil {
		t.Fatal(err)
	}
	if r := app.RejectCommand("R-missing"); r.OK || r.Code != "not_found" {
		t.Fatalf("missing id: %+v, want not_found", r)
	}
	if r := app.RejectCommand(req.ID); !r.OK {
		t.Fatalf("reject after recovery: %+v", r)
	}
}

// #4: a session-only password is dropped when the connection stops using a
// password, and TestConnectionConfig never hands it to a changed endpoint
// through a saved key-auth row.
func TestSessionPasswordDoesNotSurviveAuthSwitch(t *testing.T) {
	app := newTestApp(t)
	srvA := newSSHTestServer(t)
	srvB := newSSHTestServer(t)
	saved := addTrustedConnection(t, app, srvA.Connection("lab"))
	if err := app.secrets.Delete(context.Background(), passwordAccount(saved.ID)); err != nil {
		t.Fatal(err)
	}
	app.forgetSecrets(saved.ID)
	if r := app.UseCredentialForSession(saved.ID, sshTestPassword); !r.OK {
		t.Fatalf("session password: %+v", r)
	}

	keyAuth := saved
	keyAuth.AuthMethod = "SSH鍵認証"
	keyAuth.Credential = ""
	if r := app.SaveConnection(keyAuth); !r.OK {
		t.Fatalf("switch to key auth: %+v", r)
	}
	if _, err := app.sessionSecrets.Get(context.Background(), passwordAccount(saved.ID)); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("session password survived the switch to key auth: %v", err)
	}

	form := saved
	form.AuthMethod = "パスワード保管"
	form.Credential = ""
	form.Port = srvB.Port()
	r := app.TestConnectionConfig(form, "")
	if r.OK || r.Code != "password_required" {
		t.Fatalf("test against server B: %+v, want password_required", r)
	}
	// Same endpoint, but the saved row is key auth: still no password reuse.
	form.Port = srvA.Port()
	if r := app.TestConnectionConfig(form, ""); r.OK || r.Code != "password_required" {
		t.Fatalf("test with saved key-auth row: %+v, want password_required", r)
	}
}

// #4 (second guard): even with a session password still present, a saved
// key-auth row does not let TestConnectionConfig use it.
func TestTestConnectionConfigIgnoresSessionPasswordOfKeyRow(t *testing.T) {
	app := newTestApp(t)
	srvA := newSSHTestServer(t)
	srvB := newSSHTestServer(t)
	conn := srvA.Connection("lab")
	conn.AuthMethod = "SSH鍵認証"
	conn.Credential = ""
	if r := app.SaveConnection(conn); !r.OK {
		t.Fatalf("save key-auth: %+v", r)
	}
	var saved Connection
	for _, c := range app.GetInitialData().Connections {
		if c.Name == "lab" {
			saved = c
		}
	}
	if err := app.sessionSecrets.Set(context.Background(), passwordAccount(saved.ID), sshTestPassword); err != nil {
		t.Fatal(err)
	}
	form := saved
	form.AuthMethod = "パスワード保管"
	form.Credential = ""
	form.Port = srvB.Port()
	if r := app.TestConnectionConfig(form, ""); r.OK || r.Code != "password_required" {
		t.Fatalf("test against server B: %+v, want password_required", r)
	}
	if n := len(srvB.Execs()); n != 0 {
		t.Fatalf("server B saw %d execs", n)
	}
}
