package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// Passwords (and opted-in key passphrases) live in the SecretStore, not in
// SQLite (phase5-keychain.md §5, §8). Every SecretStore call here is made
// without a.mu, termMu or execMu held (§5.0): a Linux unlock prompt can take
// tens of seconds.

const (
	// guiCredentialTimeout bounds a keychain read started by the user (test,
	// terminal, GUI approval): long enough to answer an unlock dialog.
	guiCredentialTimeout = 60 * time.Second
	// secretWriteTimeout bounds one Set + read-back when saving.
	secretWriteTimeout = 10 * time.Second
	// keyringMigrationTimeout bounds the startup move of plaintext passwords
	// (the first Linux unlock dialog is answered within it).
	keyringMigrationTimeout = 15 * time.Second
	secretGCInterval        = time.Hour
)

// mcpCredentialTimeout bounds a read started by an agent: it fails fast with
// credential_unavailable instead of waiting for a human. Tests shorten it.
var mcpCredentialTimeout = 5 * time.Second

// credentialError is a password that could not be read from the SecretStore.
// code is the ActionResult code the GUI reacts to.
type credentialError struct {
	code string // needs_password | keyring_unavailable
	err  error
}

func (e *credentialError) Error() string {
	if e.code == "needs_password" {
		return "password not found in the keychain: " + e.err.Error()
	}
	return "keychain unavailable: " + e.err.Error()
}

func (e *credentialError) Unwrap() error { return e.err }

func newCredentialError(err error) *credentialError {
	if errors.Is(err, ErrSecretNotFound) {
		return &credentialError{code: "needs_password", err: err}
	}
	return &credentialError{code: "keyring_unavailable", err: err}
}

// credentialMessage is the user-facing text of a credentialError.
func credentialMessage(err *credentialError) string {
	if err.code == "needs_password" {
		return "保存したパスワードがキーチェーンに見つかりません。接続先画面でパスワードを入力し直してください"
	}
	return "キーチェーンを利用できないため、保存したパスワードを読み出せません。キーチェーンのロックを解除するか、このセッションだけ使うパスワードを入力してください"
}

// sessionSecretStore returns sessionSecrets as a SecretStore, or nil (a nil
// *memorySecretStore in an interface would not compare equal to nil).
func (a *App) sessionSecretStore() SecretStore {
	if a.sessionSecrets == nil {
		return nil
	}
	return a.sessionSecrets
}

func (a *App) cachedSecret(account string) (string, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	v, ok := a.secretCache[account]
	return v, ok
}

func (a *App) cacheSecret(account, secret string) {
	a.mu.Lock()
	if a.secretCache == nil {
		a.secretCache = map[string]string{}
	}
	a.secretCache[account] = secret
	a.mu.Unlock()
}

// forgetSecretsLocked drops the cached secrets of one connection. Callers
// hold a.mu.
func (a *App) forgetSecretsLocked(connID string) {
	prefix := connSecretPrefix(connID)
	for account := range a.secretCache {
		if strings.HasPrefix(account, prefix) {
			delete(a.secretCache, account)
		}
	}
}

func (a *App) forgetSecrets(connID string) {
	a.mu.Lock()
	a.forgetSecretsLocked(connID)
	a.mu.Unlock()
}

// readSecret resolves account from the cache, then the session-only store,
// then the keychain, and caches what it found.
func (a *App) readSecret(ctx context.Context, account string) (string, error) {
	if v, ok := a.cachedSecret(account); ok {
		return v, nil
	}
	v, err := lookupSecret(ctx, a.sessionSecretStore(), a.secrets, account)
	if err != nil {
		return "", err
	}
	a.cacheSecret(account, v)
	return v, nil
}

// resolveCredential fills in the secrets conn needs to dial: the keychain
// password of a password connection (credentialInKeyring), and a saved key
// passphrase when no session passphrase is set. Call it without a.mu; ctx
// bounds the keychain wait (guiCredentialTimeout / mcpCredentialTimeout).
// A missing or unreadable password is a *credentialError; an unreadable
// passphrase is not (the dial then asks for it with needs_passphrase).
func (a *App) resolveCredential(ctx context.Context, conn Connection) (Connection, error) {
	if conn.connectionType() == "Serial" || conn.ID == "" {
		return conn, nil
	}
	if credentialInKeyring(conn) {
		secret, err := a.readSecret(ctx, passwordAccount(conn.ID))
		if err != nil {
			return conn, newCredentialError(err)
		}
		conn.Credential = secret
		return conn, nil
	}
	if conn.usesPassword() && conn.Credential == "" && a.sessionSecrets != nil {
		// Never stored (the keychain was unavailable when it was saved):
		// a password given for this session only.
		if v, err := a.sessionSecrets.Get(ctx, passwordAccount(conn.ID)); err == nil {
			conn.Credential = v
		}
		return conn, nil
	}
	if conn.usesPassword() || conn.KeyPassphrase != "" || conn.Credential == "" || a.store == nil {
		return conn, nil
	}
	saved, err := a.store.passphraseSavedIDs()
	if err != nil || !saved[conn.ID] {
		return conn, nil
	}
	if v, err := a.readSecret(ctx, passphraseAccount(conn.ID, conn.Credential)); err == nil {
		conn.KeyPassphrase = v
	}
	return conn, nil
}

// collectSecretGC deletes the SecretStore items queued in secret_gc. Callers
// hold a.secretMu (so a save cannot re-reference an item between the
// referenced check and the Delete) and nothing else.
func (a *App) collectSecretGC() {
	if a.store == nil || a.secrets == nil {
		return
	}
	ctx, cancel := context.WithTimeout(a.appCtx, secretWriteTimeout)
	defer cancel()
	if _, err := a.store.runSecretGC(ctx, a.secrets); err != nil {
		log.Printf("secret gc: %v", err)
	}
}

// runSecretGCLoop retries the secret_gc queue hourly until ctx is done.
func (a *App) runSecretGCLoop(ctx context.Context) {
	defer a.wg.Done()
	ticker := time.NewTicker(secretGCInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		a.secretMu.Lock()
		a.collectSecretGC()
		a.secretMu.Unlock()
	}
}

// migrateKeychain is the startup step of §7: move plaintext passwords into
// the keychain, refuse new plaintext from then on, and retry the GC queue. It
// runs before the connections are loaded and before MCP starts. It returns
// how many passwords are still in the credential column.
func migrateKeychain(store *Store, secrets SecretStore) int {
	ctx, cancel := context.WithTimeout(context.Background(), keyringMigrationTimeout)
	defer cancel()
	result, err := store.migrateCredentialsToKeyring(ctx, secrets, secretWriteTimeout)
	if err != nil {
		log.Printf("migrate passwords to keychain: %v", err)
	}
	if result.Err != nil {
		log.Printf("keychain unavailable during migration: %v", result.Err)
	}
	// Legacy rows that could not move may still be saved unchanged; new
	// plaintext is refused (§8: no fallback to the database).
	store.denyPlaintextPasswords.Store(true)
	if secrets != nil {
		if _, err := store.runSecretGC(ctx, secrets); err != nil {
			log.Printf("secret gc: %v", err)
		}
	}
	return result.Remaining
}

// UseCredentialForSession keeps a password for this run of the app only (in
// memory), for when the keychain is unavailable or the saved item is gone.
// It is never written to SQLite or the keychain.
func (a *App) UseCredentialForSession(id string, secret string) ActionResult {
	conn, ok := a.connectionByID(strings.TrimSpace(id))
	if !ok {
		return ActionResult{OK: false, Code: "not_found", Message: "接続先が見つかりません"}
	}
	if conn.connectionType() == "Serial" || !conn.usesPassword() {
		return ActionResult{OK: false, Code: "not_password_auth", Message: "パスワード認証の接続先ではありません", ID: conn.ID}
	}
	if secret == "" {
		return ActionResult{OK: false, Code: "password_required", Message: "パスワードを入力してください", ID: conn.ID}
	}
	if a.sessionSecrets == nil {
		return ActionResult{OK: false, Code: "keyring_unavailable", Message: "このセッション用の保存先がありません", ID: conn.ID}
	}
	if err := a.sessionSecrets.Set(context.Background(), passwordAccount(conn.ID), secret); err != nil {
		return ActionResult{OK: false, Code: "keyring_unavailable", Message: fmt.Sprintf("パスワードを保持できません: %v", err), ID: conn.ID}
	}
	a.forgetSecrets(conn.ID)
	return ActionResult{OK: true, Message: fmt.Sprintf("%s のパスワードをこのセッションだけ使います（アプリを終了すると消えます）", conn.Name), ID: conn.ID}
}

// TestConnectionWithPassphraseSave is TestConnectionWithPassphrase that, when
// save is true and the test succeeds, also keeps the passphrase in the
// keychain for this key path (opt-in, phase5-keychain.md §5.4).
func (a *App) TestConnectionWithPassphraseSave(id string, passphrase string, save bool) ActionResult {
	result := a.TestConnectionWithPassphrase(id, passphrase)
	if !save || !result.OK || strings.TrimSpace(passphrase) == "" {
		return result
	}
	conn, ok := a.connectionByID(strings.TrimSpace(id))
	keyPath := keyPathOf(conn)
	if !ok || keyPath == "" || a.store == nil {
		return result
	}
	a.secretMu.Lock()
	defer a.secretMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), secretWriteTimeout)
	defer cancel()
	account := passphraseAccount(conn.ID, keyPath)
	if err := storeSecretVerified(ctx, a.secrets, account, passphrase); err != nil {
		result.Message += "（パスフレーズをキーチェーンに保存できませんでした。このセッションの間だけ使います）"
		result.Code = "keyring_unavailable"
		return result
	}
	saved, err := a.store.setPassphraseSaved(conn.ID, keyPath, true)
	if err != nil || !saved {
		// The row changed meanwhile: do not leave an unreferenced item.
		if err := a.store.enqueueSecretGC(account); err != nil {
			log.Printf("queue passphrase for gc: %v", err)
		}
		a.collectSecretGC()
		result.Message += "（接続先が変更されたため、パスフレーズは保存していません）"
		return result
	}
	a.cacheSecret(account, passphrase)
	result.Message += "（パスフレーズをキーチェーンに保存しました）"
	return result
}

// PlaintextBackups lists the files next to the database that still contain
// plaintext passwords (pre-migration backups, connections.json.migrated).
func (a *App) PlaintextBackups() []PlaintextBackup {
	if a.dbPath == "" {
		return []PlaintextBackup{}
	}
	out, err := findPlaintextBackups(a.dbPath, a.legacyConnections+".migrated")
	if err != nil {
		log.Printf("find plaintext backups: %v", err)
	}
	if out == nil {
		out = []PlaintextBackup{}
	}
	return out
}

// DeletePlaintextBackups removes the listed backups; only paths that
// PlaintextBackups reports right now are removed.
func (a *App) DeletePlaintextBackups(paths []string) ActionResult {
	if a.dbPath == "" {
		return ActionResult{OK: false, Code: "storage", Message: errStoreUnavailable.Error()}
	}
	removed, err := removePlaintextBackups(a.dbPath, paths, a.legacyConnections+".migrated")
	if err != nil {
		return ActionResult{OK: false, Code: "storage", Message: fmt.Sprintf("%d 件を削除しましたが、削除できないファイルがあります: %v", len(removed), err)}
	}
	return ActionResult{OK: true, Message: fmt.Sprintf("平文のパスワードを含むバックアップを %d 件削除しました", len(removed))}
}
