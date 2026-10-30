package obs

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

// After rotation the old ciphertext must still be readable: this is the **correctness bottom line** of rotation.
// It only changes "which key new writes use" and never deletes the old key — deleting it is irreversible data loss.
func TestRotateKeepsOldCiphertextReadable(t *testing.T) {
	svc, _ := newSvc(t, true)
	oldKeyID := svc.KeyInfo().KeyID

	_, err := svc.Ingest([]IngestItem{{
		Log:         sampleLog("snap-1", "trace-1", "REVIEW"),
		RawSnapshot: json.RawMessage(`{"request":{"amount":1500}}`),
	}})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	const newKey = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	newID, rerr := svc.RotateKey(newKey)
	if rerr != nil {
		t.Fatalf("rotate: %v", rerr)
	}
	if newID == oldKeyID {
		t.Fatalf("轮换后 keyId 未变化: %s", newID)
	}

	info := svc.KeyInfo()
	if info.KeyID != newID {
		t.Fatalf("当前钥应为 %s，实际 %s", newID, info.KeyID)
	}
	if len(info.Retained) != 1 || info.Retained[0] != oldKeyID {
		t.Fatalf("旧钥应进入保留集: %+v", info.Retained)
	}

	// Key assertion: ciphertext written before rotation (keyID=oldKeyID) still decrypts to the original.
	raw, oerr := svc.RawSnapshot("LOG-20261030-0001")
	if oerr != nil {
		t.Fatalf("轮换后旧密文应可解密，实际 %v", oerr)
	}
	if string(raw) != `{"request":{"amount":1500}}` {
		t.Fatalf("解密结果不符: %s", raw)
	}
}

// Ciphertext written after rotation must carry the new keyID: otherwise "rotation" only changed a label while still using the old key.
func TestRotateTagsNewCiphertextWithNewKeyID(t *testing.T) {
	svc, rawStore := newSvc(t, true)
	const newKey = "aa00000000000000000000000000000000000000000000000000000000000001"
	if _, rerr := svc.RotateKey(newKey); rerr != nil {
		t.Fatalf("rotate: %v", rerr)
	}
	if _, err := svc.Ingest([]IngestItem{{
		Log:         sampleLog("snap-2", "trace-2", "PASS"),
		RawSnapshot: json.RawMessage(`{"ok":true}`),
	}}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	var got string
	rawStore.View(func(d *RawDoc) {
		for _, b := range d.Blobs {
			got = b.KeyID
		}
	})
	if got != svc.KeyInfo().KeyID {
		t.Fatalf("新密文 keyID 应为当前钥 %s，实际 %s", svc.KeyInfo().KeyID, got)
	}
}

// An unknown keyID must not silently fall back to the current key: otherwise "unreadable after rotation" is disguised as "corrupted data".
func TestUnknownKeyIDFailsLoudly(t *testing.T) {
	svc, rawStore := newSvc(t, true)
	if _, err := svc.Ingest([]IngestItem{{
		Log:         sampleLog("snap-3", "trace-3", "PASS"),
		RawSnapshot: json.RawMessage(`{"x":1}`),
	}}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	// Mark the ciphertext as belonging to a key that does not exist at all.
	rawStore.Update(func(d *RawDoc) error {
		b := d.Blobs["LOG-20261030-0001"]
		b.KeyID = "k-deadbeef"
		d.Blobs["LOG-20261030-0001"] = b
		return nil
	})
	_, oerr := svc.RawSnapshot("LOG-20261030-0001")
	if oerr == nil {
		t.Fatal("未知 keyID 应报错，实际解密成功")
	}
	if oerr.Code != CodeKeyUnknown && oerr.Code != CodeDecryptFailed {
		t.Fatalf("应报密钥未知类错误，实际 %s: %s", oerr.Code, oerr.Message)
	}
}

// After re-encryption the old key is no longer required: this is the prerequisite for retiring an old key.
func TestRewrapMovesCiphertextToCurrentKey(t *testing.T) {
	svc, rawStore := newSvc(t, true)
	for i, id := range []string{"snap-a", "snap-b"} {
		if _, err := svc.Ingest([]IngestItem{{
			Log:         sampleLog(id, "trace-"+id, "PASS"),
			RawSnapshot: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i+1)),
		}}); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}
	const newKey = "bb00000000000000000000000000000000000000000000000000000000000002"
	if _, rerr := svc.RotateKey(newKey); rerr != nil {
		t.Fatalf("rotate: %v", rerr)
	}
	done, rerr := svc.RewrapRaw()
	if rerr != nil {
		t.Fatalf("rewrap: %v", rerr)
	}
	if done != 2 {
		t.Fatalf("应重加密 2 条，实际 %d", done)
	}
	cur := svc.KeyInfo().KeyID
	rawStore.View(func(d *RawDoc) {
		for id, b := range d.Blobs {
			if b.KeyID != cur {
				t.Fatalf("%s 重加密后 keyID 应为 %s，实际 %s", id, cur, b.KeyID)
			}
		}
	})
	// After re-encryption the original is still readable (swapping keys must not change the content).
	if _, oerr := svc.RawSnapshot("LOG-20261030-0001"); oerr != nil {
		t.Fatalf("重加密后应可解密: %v", oerr)
	}
	// Idempotent: running it again has nothing to do.
	again, rerr := svc.RewrapRaw()
	if rerr != nil || again != 0 {
		t.Fatalf("重复重加密应为 0 条，实际 %d / %v", again, rerr)
	}
}

// Managed source: a historical key missing locally is fetched from KMS on demand, then stored into the retained set (no repeated round-trips).
func TestProviderAdoptsMissingKeyOnce(t *testing.T) {
	box, err := NewBox(testKey)
	if err != nil {
		t.Fatalf("box: %v", err)
	}
	oldKey := make([]byte, 32)
	for i := range oldKey {
		oldKey[i] = byte(0x10 + i)
	}
	// First create some ciphertext with the old key (simulating historical data).
	oldBox, _ := newBoxFromKey(oldKey, "static")
	nonce, ct, err := oldBox.Seal([]byte(`{"legacy":true}`), "LOG-1")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	p := &countingProvider{encoded: keyHex(oldKey)}
	box.WithKeyProvider(p)
	plain, oerr := box.OpenWithKeyID(nonce, ct, "LOG-1", oldBox.KeyID())
	if oerr != nil {
		t.Fatalf("应按需取回历史钥并解密: %v", oerr)
	}
	if string(plain) != `{"legacy":true}` {
		t.Fatalf("解密结果不符: %s", plain)
	}
	if p.calls != 1 {
		t.Fatalf("托管源应只被调用 1 次，实际 %d", p.calls)
	}
	if ids := box.RetainedKeyIDs(); len(ids) != 1 || ids[0] != oldBox.KeyID() {
		t.Fatalf("取回的钥匙应落入保留集: %+v", ids)
	}
	// The second decryption must not hit the managed source again.
	if _, oerr := box.OpenWithKeyID(nonce, ct, "LOG-1", oldBox.KeyID()); oerr != nil {
		t.Fatalf("第二次解密失败: %v", oerr)
	}
	if p.calls != 1 {
		t.Fatalf("同一把历史钥不应重复取，实际调用 %d 次", p.calls)
	}
}

// RotateFromProvider: KMS returns a new key → rotate; returns the same key → no-op.
func TestRotateFromProvider(t *testing.T) {
	box, err := NewBox(testKey)
	if err != nil {
		t.Fatalf("box: %v", err)
	}
	same := &countingProvider{encoded: testKey}
	if _, rerr := box.RotateFromProvider(); rerr == nil {
		t.Fatal("未挂托管源应报错")
	}
	box.WithKeyProvider(same)
	if id, rerr := box.RotateFromProvider(); rerr != nil || id != keyIDOf(mustKey(t, testKey)) {
		t.Fatalf("同钥轮换应无变化: %s %v", id, rerr)
	}
	if len(box.RetainedKeyIDs()) != 0 {
		t.Fatalf("同钥轮换不应产生保留钥: %+v", box.RetainedKeyIDs())
	}

	newKey := make([]byte, 32)
	for i := range newKey {
		newKey[i] = 0x7f
	}
	box.WithKeyProvider(&countingProvider{encoded: keyHex(newKey)})
	id, rerr := box.RotateFromProvider()
	if rerr != nil {
		t.Fatalf("轮换: %v", rerr)
	}
	if id != box.KeyID() || id != keyIDOf(newKey) {
		t.Fatalf("轮换后 keyId 应为 %s，实际 %s / %s", keyIDOf(newKey), id, box.KeyID())
	}
}

// When the managed source returns a key that does not match the requested keyID it must be rejected: trying the wrong key only yields
// a GCM verification failure and disguises "wrong key fetched" as "corrupted data".
func TestProviderMismatchIsRejected(t *testing.T) {
	box, _ := NewBox(testKey)
	box.WithKeyProvider(&countingProvider{encoded: testKey})
	_, oerr := box.OpenWithKeyID("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", "AAAA", "LOG-1", "k-unknown")
	if oerr == nil {
		t.Fatal("钥匙不匹配应报错")
	}
	var oe *Error
	if !errors.As(oerr, &oe) || oe.Code != CodeKeyUnknown {
		t.Fatalf("应为 %s，实际 %v", CodeKeyUnknown, oerr)
	}
}

// With no key configured, KeyInfo must explicitly return nil rather than an empty shell that suggests "there is a key but it is broken".
func TestKeyInfoNilWithoutKey(t *testing.T) {
	svc, _ := newSvc(t, false)
	if svc.KeyInfo() != nil {
		t.Fatalf("未配密钥时 KeyInfo 应为 nil: %+v", svc.KeyInfo())
	}
	if _, rerr := svc.RotateKey(testKey); rerr == nil || rerr.Code != CodeKeyMissing {
		t.Fatalf("未配密钥轮换应报 %s，实际 %v", CodeKeyMissing, rerr)
	}
	if _, rerr := svc.RewrapRaw(); rerr == nil || rerr.Code != CodeKeyMissing {
		t.Fatalf("未配密钥重加密应报 %s，实际 %v", CodeKeyMissing, rerr)
	}
}

// Rotating to an invalid key must not corrupt the key in memory (validate first, then swap).
func TestRotateRejectsBadKeyAndKeepsCurrent(t *testing.T) {
	svc, _ := newSvc(t, true)
	before := svc.KeyInfo().KeyID
	for _, bad := range []string{"", "  ", "not-a-key", "0011"} {
		if _, rerr := svc.RotateKey(bad); rerr == nil {
			t.Fatalf("非法密钥 %q 应被拒绝", bad)
		}
	}
	if svc.KeyInfo().KeyID != before {
		t.Fatalf("非法轮换不应改动当前钥: %s → %s", before, svc.KeyInfo().KeyID)
	}
}

// ---------------------------------------------------------------------------

type countingProvider struct {
	encoded string
	calls   int
}

func (p *countingProvider) Name() string { return "test-kms" }

func (p *countingProvider) Fetch(keyID string) ([]byte, string, string, error) {
	p.calls++
	sp := StaticKeyProvider{Encoded: p.encoded, Source: "test-kms"}
	return sp.Fetch(keyID)
}

func keyHex(key []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 0, len(key)*2)
	for _, b := range key {
		out = append(out, hexdigits[b>>4], hexdigits[b&0x0f])
	}
	return string(out)
}

func mustKey(t *testing.T, encoded string) []byte {
	t.Helper()
	k, err := decodeKey(encoded)
	if err != nil {
		t.Fatalf("decodeKey: %v", err)
	}
	return k
}
