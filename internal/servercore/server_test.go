package servercore

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"

	"lanmessenger/internal/crypto"
	"lanmessenger/internal/proto"
)

const testPassphrase = "kitchen table 42"

func newTestServer(t *testing.T, requireApproval bool) string {
	t.Helper()
	dir := t.TempDir()

	verifier, err := crypto.NewPassphraseVerifier(testPassphrase)
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	cfg := &Config{
		DataDir:              dir,
		Passphrase:           verifier,
		RequireAdminApproval: requireApproval,
		HeartbeatSeconds:     3600, // don't ping during short tests
	}
	cfg.SetPath(dir + "/server.toml")
	cfg.applyDefaults()

	srv, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.serve(ctx, ln); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	})
	return ln.Addr().String()
}

// testClient is a minimal protocol client for tests.
type testClient struct {
	t        *testing.T
	ws       *websocket.Conn
	id       *crypto.Identity
	deviceID string
}

func dial(t *testing.T, addr string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "wss://"+addr+"/ws", &websocket.DialOptions{
		HTTPClient: httpClientInsecure,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	ws.SetReadLimit(2 << 20)
	return ws
}

func (c *testClient) send(typ proto.Type, payload any) {
	c.t.Helper()
	env, err := proto.NewEnvelope(typ, "", time.Now().UnixMilli(), payload)
	if err != nil {
		c.t.Fatalf("build %s: %v", typ, err)
	}
	raw, _ := proto.Marshal(env)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageText, raw); err != nil {
		c.t.Fatalf("write %s: %v", typ, err)
	}
}

func (c *testClient) recv() *proto.Envelope {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := c.ws.Read(ctx)
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}
	env, err := proto.Parse(data)
	if err != nil {
		c.t.Fatalf("parse: %v", err)
	}
	return env
}

// recvType reads frames until one of type typ arrives, failing on an error frame
// or timeout.
func (c *testClient) recvType(typ proto.Type) *proto.Envelope {
	c.t.Helper()
	for i := 0; i < 20; i++ {
		env := c.recv()
		switch env.Type {
		case typ:
			return env
		case proto.TypeError:
			var e proto.ErrorBody
			_ = env.Unmarshal(&e)
			c.t.Fatalf("wanted %s, got error frame: %s %s", typ, e.Code, e.Message)
		default:
			// skip presence/directory/etc noise
		}
	}
	c.t.Fatalf("did not receive %s after 20 frames", typ)
	return nil
}

// authenticate runs hello + challenge/response. enroll==true also registers a
// fresh identity; otherwise it resumes an existing deviceID.
func (c *testClient) authenticate(enroll bool, displayName string) {
	c.t.Helper()
	hello := proto.Hello{ClientVersion: "test"}
	if !enroll {
		hello.DeviceID = c.deviceID
	}
	c.send(proto.TypeHello, hello)

	ch := c.recvType(proto.TypeAuthChallenge)
	var chal proto.AuthChallenge
	if err := ch.Unmarshal(&chal); err != nil {
		c.t.Fatalf("challenge: %v", err)
	}
	salt, _ := base64.StdEncoding.DecodeString(chal.Salt)
	nonce, _ := base64.StdEncoding.DecodeString(chal.Nonce)
	proof := crypto.Proof(crypto.DeriveKey(testPassphrase, salt), nonce)
	c.send(proto.TypeAuthResponse, proto.AuthResponse{
		Proof: base64.StdEncoding.EncodeToString(proof),
	})

	if enroll {
		c.send(proto.TypeEnroll, proto.Enroll{
			DisplayName: displayName,
			SignPub:     crypto.EncodeSignPub(c.id.SignPub),
			BoxPub:      crypto.EncodeKey(c.id.BoxPub),
		})
		res := c.recvType(proto.TypeEnrollResult)
		var er proto.EnrollResult
		_ = res.Unmarshal(&er)
		c.deviceID = er.DeviceID
		if er.State != proto.StateActive {
			return // pending: caller handles
		}
	}
}

func newClient(t *testing.T, addr string) *testClient {
	id, err := crypto.GenerateIdentity()
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	return &testClient{t: t, ws: dial(t, addr), id: id}
}

func (c *testClient) close() { _ = c.ws.Close(websocket.StatusNormalClosure, "") }

// httpClientInsecure trusts the server's self-signed test certificate.
var httpClientInsecure = &http.Client{
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	},
}

func TestEnrollAndRelay(t *testing.T) {
	addr := newTestServer(t, false)

	dad := newClient(t, addr)
	defer dad.close()
	dad.authenticate(true, "Dad")
	ready := dad.recvType(proto.TypeReady)
	var rd proto.Ready
	_ = ready.Unmarshal(&rd)
	if !rd.Admin {
		t.Fatal("first device should be admin")
	}
	dad.recvType(proto.TypeDirectorySnapshot)

	mom := newClient(t, addr)
	defer mom.close()
	mom.authenticate(true, "Mom")
	mom.recvType(proto.TypeReady)
	mom.recvType(proto.TypeDirectorySnapshot)

	// Dad should learn about Mom via a directory update.
	du := dad.recvType(proto.TypeDirectoryUpdate)
	var dup proto.DirectoryUpdate
	_ = du.Unmarshal(&dup)
	if dup.Entry.DisplayName != "Mom" {
		t.Fatalf("unexpected directory update: %+v", dup.Entry)
	}

	// Mom -> Dad message.
	mom.send(proto.TypeMsg, proto.Msg{
		To:         dad.deviceID,
		MsgID:      "msg-1",
		Nonce:      "bm9uY2U=",
		Ciphertext: "Y2lwaGVydGV4dA==",
		TS:         time.Now().UnixMilli(),
	})
	got := dad.recvType(proto.TypeMsg)
	var m proto.Msg
	_ = got.Unmarshal(&m)
	if m.From != mom.deviceID || m.MsgID != "msg-1" {
		t.Fatalf("relayed message wrong: %+v", m)
	}
}

func TestOfflineQueue(t *testing.T) {
	addr := newTestServer(t, false)

	dad := newClient(t, addr)
	dad.authenticate(true, "Dad")
	dad.recvType(proto.TypeReady)
	dad.recvType(proto.TypeDirectorySnapshot)

	mom := newClient(t, addr)
	defer mom.close()
	mom.authenticate(true, "Mom")
	mom.recvType(proto.TypeReady)
	mom.recvType(proto.TypeDirectorySnapshot)

	// Dad goes offline.
	dad.close()
	time.Sleep(100 * time.Millisecond)

	mom.send(proto.TypeMsg, proto.Msg{
		To: dad.deviceID, MsgID: "queued-1", Nonce: "bg==", Ciphertext: "Yw==", TS: time.Now().UnixMilli(),
	})
	time.Sleep(100 * time.Millisecond)

	// Dad reconnects with the same identity.
	dad2 := &testClient{t: t, ws: dial(t, addr), id: dad.id, deviceID: dad.deviceID}
	defer dad2.close()
	dad2.authenticate(false, "")
	dad2.recvType(proto.TypeReady)
	dad2.recvType(proto.TypeDirectorySnapshot)

	got := dad2.recvType(proto.TypeMsg)
	var m proto.Msg
	_ = got.Unmarshal(&m)
	if m.MsgID != "queued-1" || m.From != mom.deviceID {
		t.Fatalf("did not get queued message: %+v", m)
	}

	// Ack it, reconnect again, and confirm it is not redelivered.
	dad2.send(proto.TypeMsgAck, proto.MsgAck{MsgID: "queued-1", Peer: mom.deviceID})
	time.Sleep(100 * time.Millisecond)
	dad2.close()

	dad3 := &testClient{t: t, ws: dial(t, addr), id: dad.id, deviceID: dad.deviceID}
	defer dad3.close()
	dad3.authenticate(false, "")
	dad3.recvType(proto.TypeReady)
	dad3.recvType(proto.TypeDirectorySnapshot)
	// Expect no msg frame; send a ping and require a pong before any msg.
	dad3.send(proto.TypePing, nil)
	env := dad3.recv()
	for env.Type == proto.TypePresenceUpdate || env.Type == proto.TypeDirectoryUpdate {
		env = dad3.recv()
	}
	if env.Type == proto.TypeMsg {
		t.Fatal("acked message was redelivered")
	}
	if env.Type != proto.TypePong {
		t.Fatalf("expected pong, got %s", env.Type)
	}
}

func TestAdminApprovalFlow(t *testing.T) {
	addr := newTestServer(t, true)

	dad := newClient(t, addr)
	defer dad.close()
	dad.authenticate(true, "Dad")
	dad.recvType(proto.TypeReady) // first device is admin even with approval on
	dad.recvType(proto.TypeDirectorySnapshot)

	kid := newClient(t, addr)
	defer kid.close()
	kid.authenticate(true, "Kid") // returns early: state pending

	// Dad lists pending devices.
	dad.send(proto.TypeAdminListPending, proto.AdminListPending{})
	pl := dad.recvType(proto.TypeAdminPendingList)
	var list proto.AdminPendingList
	_ = pl.Unmarshal(&list)
	if len(list.Entries) != 1 || list.Entries[0].DisplayName != "Kid" {
		t.Fatalf("pending list wrong: %+v", list.Entries)
	}
	kidID := list.Entries[0].DeviceID

	// Dad approves, signing "approve:<kidID>" with his signing key.
	sig := crypto.Sign(dad.id.SignPriv, proto.AdminActionMessage("approve", kidID))
	dad.send(proto.TypeAdminApprove, proto.AdminApprove{DeviceID: kidID, Signature: sig})

	// Kid, still connected, should be promoted to ready.
	kid.recvType(proto.TypeReady)
	kid.recvType(proto.TypeDirectorySnapshot)
}

func TestBadPassphraseRejected(t *testing.T) {
	addr := newTestServer(t, false)
	c := newClient(t, addr)
	defer c.close()

	c.send(proto.TypeHello, proto.Hello{ClientVersion: "test"})
	ch := c.recvType(proto.TypeAuthChallenge)
	var chal proto.AuthChallenge
	_ = ch.Unmarshal(&chal)
	nonce, _ := base64.StdEncoding.DecodeString(chal.Nonce)
	salt, _ := base64.StdEncoding.DecodeString(chal.Salt)
	badProof := crypto.Proof(crypto.DeriveKey("wrong passphrase", salt), nonce)
	c.send(proto.TypeAuthResponse, proto.AuthResponse{Proof: base64.StdEncoding.EncodeToString(badProof)})

	// Expect an error frame, then the connection to close.
	env := c.recv()
	if env.Type != proto.TypeError {
		t.Fatalf("expected error frame, got %s", env.Type)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, _, err := c.ws.Read(ctx); err == nil {
		t.Fatal("expected connection to be closed after bad passphrase")
	}
}
