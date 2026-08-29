package clientcore

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"lanmessenger/internal/proto"
)

// chunkSize is the plaintext size of each file chunk. Sealed and base64-encoded
// it stays comfortably under the relay's default 2 MiB frame limit.
const chunkSize = 512 * 1024

// FileMeta is the JSON body stored in a history [Message] of kind
// proto.InnerFileOffer. It describes a file transfer and its outcome.
type FileMeta struct {
	TransferID string `json:"transfer_id"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	MIME       string `json:"mime,omitempty"`
	SHA256     string `json:"sha256"`
	Status     string `json:"status"` // sending | receiving | received | failed
	Path       string `json:"path,omitempty"`
}

// DecodeFileMeta parses the body of a file-transfer history message.
func DecodeFileMeta(body string) (FileMeta, error) {
	var m FileMeta
	err := json.Unmarshal([]byte(body), &m)
	return m, err
}

func encodeFileMeta(m FileMeta) string {
	b, _ := json.Marshal(m)
	return string(b)
}

// inboundTransfer tracks a file being received.
type inboundTransfer struct {
	meta      proto.FileOfferBody
	peerID    string
	partPath  string
	finalPath string
	file      *os.File
	hasher    hash.Hash
	nextIndex int
	done      int64
	msgID     string // history message id (== transfer id)
}

// SendFile encrypts and sends a file to peerID: a file_offer describing it,
// then the file split into sealed chunks. The client must be connected.
// It returns the transfer ID; progress arrives as EventFileProgress events.
func (c *Client) SendFile(ctx context.Context, peerID, path string) (string, error) {
	peer, err := c.store.getPeer(peerID)
	if err != nil {
		return "", ErrUnknownPeer
	}
	w := c.currentConn()
	if w == nil || c.State() != StateReady {
		return "", ErrNotReady
	}

	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("clientcore: stat file: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("clientcore: %s is a directory", path)
	}
	if info.Size() > c.cfg.MaxFile() {
		return "", fmt.Errorf("clientcore: file is %d bytes, over the %d byte limit", info.Size(), c.cfg.MaxFile())
	}

	sum, err := hashFile(path)
	if err != nil {
		return "", err
	}

	name := filepath.Base(path)
	mimeType := mime.TypeByExtension(filepath.Ext(path))
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	chunks := int((info.Size() + chunkSize - 1) / chunkSize)
	if chunks == 0 {
		chunks = 1 // a zero-byte file still sends one empty chunk
	}
	transferID := randID()

	offer := proto.FileOfferBody{
		TransferID: transferID,
		Name:       name,
		Size:       info.Size(),
		MIME:       mimeType,
		SHA256:     sum,
		ChunkSize:  chunkSize,
		Chunks:     chunks,
	}

	// Record it in history up front so the UI can show a progress row.
	meta := FileMeta{
		TransferID: transferID, Name: name, Size: info.Size(), MIME: mimeType,
		SHA256: sum, Status: "sending", Path: path,
	}
	histMsg := Message{
		MsgID: transferID, PeerID: peerID, Direction: DirOut,
		Kind: proto.InnerFileOffer, Body: encodeFileMeta(meta),
		TS: nowMillis(), State: StateSent,
	}
	if _, err := c.store.insertMessage(histMsg); err != nil {
		return "", err
	}
	hm := histMsg
	c.emit(Event{Kind: EventMessage, PeerID: peerID, Message: &hm})

	if err := c.sendInnerMsg(ctx, w, peer, proto.InnerFileOffer, offer); err != nil {
		c.failOutboundTransfer(peerID, meta, err)
		return "", err
	}

	f, err := os.Open(path)
	if err != nil {
		c.failOutboundTransfer(peerID, meta, err)
		return "", err
	}
	defer f.Close()

	buf := make([]byte, chunkSize)
	var sent int64
	for idx := 0; idx < chunks; idx++ {
		n, readErr := io.ReadFull(f, buf)
		if readErr == io.ErrUnexpectedEOF || readErr == io.EOF {
			readErr = nil // last (short) chunk
		}
		if readErr != nil {
			c.failOutboundTransfer(peerID, meta, readErr)
			return "", readErr
		}
		chunk := proto.FileChunkBody{
			TransferID: transferID,
			Index:      idx,
			Bytes:      base64.StdEncoding.EncodeToString(buf[:n]),
		}
		if err := c.sendInnerMsg(ctx, w, peer, proto.InnerFileChunk, chunk); err != nil {
			c.failOutboundTransfer(peerID, meta, err)
			return "", err
		}
		sent += int64(n)
		c.emit(Event{Kind: EventFileProgress, PeerID: peerID, Progress: &FileProgress{
			TransferID: transferID, Name: name, Direction: DirOut, Done: sent, Total: info.Size(),
		}})
	}

	meta.Status = "received" // fully handed to the relay; peer-side verified separately
	_ = c.updateFileMeta(peerID, DirOut, meta)
	c.emit(Event{Kind: EventFileProgress, PeerID: peerID, Progress: &FileProgress{
		TransferID: transferID, Name: name, Direction: DirOut, Done: info.Size(), Total: info.Size(), Complete: true,
	}})
	return transferID, nil
}

// sendInnerMsg seals an inner payload for peer and sends it as one relay msg.
func (c *Client) sendInnerMsg(ctx context.Context, w *wsConn, peer Peer, kind proto.InnerKind, payload any) error {
	inner, err := proto.NewInner(kind, payload)
	if err != nil {
		return err
	}
	nonceB64, ctB64, err := c.sealInner(peer, inner)
	if err != nil {
		return err
	}
	return w.send(ctx, proto.TypeMsg, "", proto.Msg{
		To: peer.DeviceID, MsgID: randID(), Nonce: nonceB64, Ciphertext: ctB64, TS: nowMillis(),
	})
}

func (c *Client) failOutboundTransfer(peerID string, meta FileMeta, cause error) {
	meta.Status = "failed"
	_ = c.updateFileMeta(peerID, DirOut, meta)
	c.emitError(fmt.Errorf("file transfer %q failed: %w", meta.Name, cause))
}

// --- receiving --------------------------------------------------------

func (c *Client) handleFileOffer(ctx context.Context, w *wsConn, peer Peer, m proto.Msg, inner *proto.Inner) {
	var offer proto.FileOfferBody
	if err := inner.Unmarshal(&offer); err != nil {
		c.emitError(err)
		c.ack(ctx, w, m.MsgID, m.From)
		return
	}
	c.ack(ctx, w, m.MsgID, m.From)

	if offer.Size > c.cfg.MaxFile() {
		c.emitError(fmt.Errorf("declining %q from %s: %d bytes exceeds the limit", offer.Name, peer.DisplayName, offer.Size))
		return
	}

	downloads, err := c.cfg.ResolvedDownloadsDir()
	if err != nil {
		c.emitError(err)
		return
	}
	finalPath := uniquePath(filepath.Join(downloads, sanitizeName(offer.Name)))
	partPath := finalPath + ".part"
	f, err := os.Create(partPath)
	if err != nil {
		c.emitError(fmt.Errorf("clientcore: create download file: %w", err))
		return
	}

	tr := &inboundTransfer{
		meta: offer, peerID: m.From, partPath: partPath, finalPath: finalPath,
		file: f, hasher: sha256.New(), msgID: offer.TransferID,
	}
	c.transfersMu.Lock()
	c.transfers[offer.TransferID] = tr
	c.transfersMu.Unlock()

	meta := FileMeta{
		TransferID: offer.TransferID, Name: offer.Name, Size: offer.Size, MIME: offer.MIME,
		SHA256: offer.SHA256, Status: "receiving", Path: finalPath,
	}
	hist := Message{
		MsgID: offer.TransferID, PeerID: m.From, Direction: DirIn,
		Kind: proto.InnerFileOffer, Body: encodeFileMeta(meta), TS: m.TS, State: StateReceived,
	}
	if _, err := c.store.insertMessage(hist); err == nil {
		hm := hist
		c.emit(Event{Kind: EventMessage, PeerID: m.From, Message: &hm})
	}
	c.emit(Event{Kind: EventFileProgress, PeerID: m.From, Progress: &FileProgress{
		TransferID: offer.TransferID, Name: offer.Name, Direction: DirIn, Done: 0, Total: offer.Size,
	}})
}

func (c *Client) handleFileChunk(ctx context.Context, w *wsConn, m proto.Msg, inner *proto.Inner) {
	var chunk proto.FileChunkBody
	if err := inner.Unmarshal(&chunk); err != nil {
		c.emitError(err)
		c.ack(ctx, w, m.MsgID, m.From)
		return
	}
	c.ack(ctx, w, m.MsgID, m.From)

	c.transfersMu.Lock()
	tr := c.transfers[chunk.TransferID]
	c.transfersMu.Unlock()
	if tr == nil {
		c.emitError(fmt.Errorf("clientcore: file chunk for unknown transfer %s", chunk.TransferID))
		return
	}

	if chunk.Index != tr.nextIndex {
		c.abortInbound(tr, fmt.Errorf("chunks arrived out of order (got %d, want %d)", chunk.Index, tr.nextIndex))
		return
	}
	raw, err := base64.StdEncoding.DecodeString(chunk.Bytes)
	if err != nil {
		c.abortInbound(tr, fmt.Errorf("bad chunk encoding: %w", err))
		return
	}
	if _, err := tr.file.Write(raw); err != nil {
		c.abortInbound(tr, err)
		return
	}
	tr.hasher.Write(raw)
	tr.nextIndex++
	tr.done += int64(len(raw))

	c.emit(Event{Kind: EventFileProgress, PeerID: tr.peerID, Progress: &FileProgress{
		TransferID: tr.meta.TransferID, Name: tr.meta.Name, Direction: DirIn,
		Done: tr.done, Total: tr.meta.Size,
	}})

	if tr.nextIndex < tr.meta.Chunks {
		return
	}

	// All chunks in: finalize.
	c.transfersMu.Lock()
	delete(c.transfers, chunk.TransferID)
	c.transfersMu.Unlock()
	_ = tr.file.Close()

	got := hex.EncodeToString(tr.hasher.Sum(nil))
	meta := FileMeta{
		TransferID: tr.meta.TransferID, Name: tr.meta.Name, Size: tr.meta.Size,
		MIME: tr.meta.MIME, SHA256: tr.meta.SHA256, Path: tr.finalPath,
	}
	if got != tr.meta.SHA256 {
		_ = os.Remove(tr.partPath)
		meta.Status = "failed"
		_ = c.updateFileMeta(tr.peerID, DirIn, meta)
		c.emitError(fmt.Errorf("clientcore: %q failed integrity check (sha256 mismatch)", tr.meta.Name))
		return
	}
	if err := os.Rename(tr.partPath, tr.finalPath); err != nil {
		meta.Status = "failed"
		_ = c.updateFileMeta(tr.peerID, DirIn, meta)
		c.emitError(fmt.Errorf("clientcore: could not move completed download: %w", err))
		return
	}
	meta.Status = "received"
	_ = c.updateFileMeta(tr.peerID, DirIn, meta)
	c.emit(Event{Kind: EventFileProgress, PeerID: tr.peerID, Progress: &FileProgress{
		TransferID: tr.meta.TransferID, Name: tr.meta.Name, Direction: DirIn,
		Done: tr.meta.Size, Total: tr.meta.Size, Complete: true, Path: tr.finalPath,
	}})
}

func (c *Client) abortInbound(tr *inboundTransfer, cause error) {
	c.transfersMu.Lock()
	delete(c.transfers, tr.meta.TransferID)
	c.transfersMu.Unlock()
	_ = tr.file.Close()
	_ = os.Remove(tr.partPath)

	meta := FileMeta{
		TransferID: tr.meta.TransferID, Name: tr.meta.Name, Size: tr.meta.Size,
		MIME: tr.meta.MIME, SHA256: tr.meta.SHA256, Status: "failed", Path: tr.finalPath,
	}
	_ = c.updateFileMeta(tr.peerID, DirIn, meta)
	c.emitError(fmt.Errorf("clientcore: incoming file %q aborted: %w", tr.meta.Name, cause))
}

// updateFileMeta rewrites the body of a file-transfer history message.
func (c *Client) updateFileMeta(peerID string, dir Direction, meta FileMeta) error {
	_, err := c.store.db.Exec(
		`UPDATE messages SET body = ? WHERE msg_id = ? AND peer_id = ? AND direction = ?`,
		encodeFileMeta(meta), meta.TransferID, peerID, string(dir))
	return err
}

// --- helpers ---------------------------------------------------------

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("clientcore: open for hashing: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("clientcore: hash file: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sanitizeName strips any directory components and control characters from a
// sender-supplied filename so it can't escape the downloads directory.
func sanitizeName(name string) string {
	name = filepath.Base(filepath.FromSlash(name))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "download"
	}
	return name
}

// uniquePath returns path if free, otherwise "name (2).ext", "name (3).ext", ...
func uniquePath(path string) string {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return path
	}
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand
		}
	}
}
