// Copyright (C) 2014 The Protocol Authors.

package protocol

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"time"

	lz4 "github.com/bkaradzic/go-lz4"
)

const (
	// Shifts
	KiB = 10
	MiB = 20
	GiB = 30
)

const (
	// MaxMessageLen is the largest message size allowed on the wire. (500 MB)
	MaxMessageLen = 500 * 1000 * 1000

	// MinBlockSize is the minimum block size allowed
	MinBlockSize = 128 << KiB

	// MaxBlockSize is the maximum block size allowed
	MaxBlockSize = 16 << MiB

	// DesiredPerFileBlocks is the number of blocks we aim for per file
	DesiredPerFileBlocks = 2000
)

// BlockSizes is the list of valid block sizes, from min to max
var BlockSizes []int

// For each block size, the hash of a block of all zeroes
var sha256OfEmptyBlock = make(map[int][sha256.Size]byte)

func init() {
	for blockSize := MinBlockSize; blockSize <= MaxBlockSize; blockSize *= 2 {
		BlockSizes = append(BlockSizes, blockSize)
		sha256OfEmptyBlock[blockSize] = sha256.Sum256(make([]byte, blockSize))
	}
	BufferPool = newBufferPool()
}

// BlockSize returns the block size to use for the given file size
func BlockSize(fileSize int64) int {
	var blockSize int
	for _, blockSize = range BlockSizes {
		if fileSize < DesiredPerFileBlocks*int64(blockSize) {
			break
		}
	}

	return blockSize
}

const (
	stateInitial = iota
	stateReady
)

// Request message flags
const (
	FlagFromTemporary uint32 = 1 << iota
)

// ClusterConfigMessage.Folders flags
const (
	FlagFolderReadOnly            uint32 = 1 << 0
	FlagFolderIgnorePerms                = 1 << 1
	FlagFolderIgnoreDelete               = 1 << 2
	FlagFolderDisabledTempIndexes        = 1 << 3
	FlagFolderAll                        = 1<<4 - 1
)

// ClusterConfigMessage.Folders.Devices flags
const (
	FlagShareTrusted  uint32 = 1 << 0
	FlagShareReadOnly        = 1 << 1
	FlagIntroducer           = 1 << 2
	FlagShareBits            = 0x000000ff
)

// FileInfo.LocalFlags flags
const (
	FlagLocalUnsupported = 1 << 0 // The kind is unsupported, e.g. symlinks on Windows
	FlagLocalIgnored     = 1 << 1 // Matches local ignore patterns
	FlagLocalMustRescan  = 1 << 2 // Doesn't match content on disk, must be rechecked fully
	FlagLocalReceiveOnly = 1 << 3 // Change detected on receive only folder

	// Flags that should result in the Invalid bit on outgoing updates
	LocalInvalidFlags = FlagLocalUnsupported | FlagLocalIgnored | FlagLocalMustRescan | FlagLocalReceiveOnly

	// Flags that should result in a file being in conflict with its
	// successor, due to us not having an up to date picture of its state on
	// disk.
	LocalConflictFlags = FlagLocalUnsupported | FlagLocalIgnored | FlagLocalReceiveOnly

	LocalAllFlags = FlagLocalUnsupported | FlagLocalIgnored | FlagLocalMustRescan | FlagLocalReceiveOnly
)

var (
	ErrClosed               = errors.New("connection closed")
	ErrTimeout              = errors.New("read timeout")
	ErrSwitchingConnections = errors.New("switching connections")
	errUnknownMessage       = errors.New("unknown message")
	errInvalidFilename      = errors.New("filename is invalid")
	errUncleanFilename      = errors.New("filename not in canonical format")
	errDeletedHasBlocks     = errors.New("deleted file with non-empty block list")
	errDirectoryHasBlocks   = errors.New("directory with non-empty block list")
	errFileHasNoBlocks      = errors.New("file with empty block list")
)

type Model interface {
	// An index was received from the peer device
	Index(deviceID DeviceID, folder string, files []FileInfo)
	// An index update was received from the peer device
	IndexUpdate(deviceID DeviceID, folder string, files []FileInfo)
	// A request was made by the peer device
	Request(deviceID DeviceID, folder, name string, size int32, offset int64, hash []byte, weakHash uint32, fromTemporary bool) (RequestResponse, error)
	// A cluster configuration message was received
	ClusterConfig(deviceID DeviceID, config ClusterConfig)
	// The peer device closed the connection
	Closed(conn Connection, err error)
	// The peer device sent progress updates for the files it is currently downloading
	DownloadProgress(deviceID DeviceID, folder string, updates []FileDownloadProgressUpdate)
}

type RequestResponse interface {
	Data() []byte
	Close() // Must always be called once the byte slice is no longer in use
	Wait()  // Blocks until Close is called
}

type Connection interface {
	Start()
	ID() DeviceID
	Name() string
	Index(folder string, files []FileInfo) error
	IndexUpdate(folder string, files []FileInfo) error
	Request(folder string, name string, offset int64, size int, hash []byte, weakHash uint32, fromTemporary bool) ([]byte, error)
	ClusterConfig(config ClusterConfig)
	DownloadProgress(folder string, updates []FileDownloadProgressUpdate)
	Statistics() Statistics
	Closed() bool
}

type rawConnection struct {
	id       DeviceID
	name     string
	receiver Model

	cr *countingReader
	cw *countingWriter

	awaiting    map[int32]chan asyncResult
	awaitingMut sync.Mutex

	idxMut sync.Mutex // ensures serialization of Index calls

	nextID    int32
	nextIDMut sync.Mutex

	outbox      chan asyncMessage
	closed      chan struct{}
	once        sync.Once
	compression Compression
}

type asyncResult struct {
	val []byte
	err error
}

type message interface {
	ProtoSize() int
	Marshal() ([]byte, error)
	MarshalTo([]byte) (int, error)
	Unmarshal([]byte) error
}

type asyncMessage struct {
	msg  message
	done chan struct{} // done closes when we're done sending the message
}

const (
	// PingSendInterval is how often we make sure to send a message, by
	// triggering pings if necessary.
	PingSendInterval = 90 * time.Second
	// ReceiveTimeout is the longest we'll wait for a message from the other
	// side before closing the connection.
	ReceiveTimeout = 300 * time.Second
)

func NewConnection(deviceID DeviceID, reader io.Reader, writer io.Writer, receiver Model, name string, compress Compression) Connection {
	cr := &countingReader{Reader: reader}
	cw := &countingWriter{Writer: writer}

	c := rawConnection{
		id:          deviceID,
		name:        name,
		receiver:    nativeModel{receiver},
		cr:          cr,
		cw:          cw,
		awaiting:    make(map[int32]chan asyncResult),
		outbox:      make(chan asyncMessage),
		closed:      make(chan struct{}),
		compression: compress,
	}

	return wireFormatConnection{&c}
}

// Start creates the goroutines for sending and receiving of messages. It must
// be called exactly once after creating a connection.
func (c *rawConnection) Start() {
	go c.readerLoop()
	go c.writerLoop()
	go c.pingSender()
	go c.pingReceiver()
}

func (c *rawConnection) ID() DeviceID {
	return c.id
}

func (c *rawConnection) Name() string {
	return c.name
}

// Index writes the list of file information to the connected peer device
func (c *rawConnection) Index(folder string, idx []FileInfo) error {
	select {
	case <-c.closed:
		return ErrClosed
	default:
	}
	c.idxMut.Lock()
	c.send(&Index{
		Folder: folder,
		Files:  idx,
	}, nil)
	c.idxMut.Unlock()
	return nil
}

// IndexUpdate writes the list of file information to the connected peer device as an update
func (c *rawConnection) IndexUpdate(folder string, idx []FileInfo) error {
	select {
	case <-c.closed:
		return ErrClosed
	default:
	}
	c.idxMut.Lock()
	c.send(&IndexUpdate{
		Folder: folder,
		Files:  idx,
	}, nil)
	c.idxMut.Unlock()
	return nil
}

// Request returns the bytes for the specified block after fetching them from the connected peer.
func (c *rawConnection) Request(folder string, name string, offset int64, size int, hash []byte, weakHash uint32, fromTemporary bool) ([]byte, error) {
	c.nextIDMut.Lock()
	id := c.nextID
	c.nextID++
	c.nextIDMut.Unlock()

	c.awaitingMut.Lock()
	if _, ok := c.awaiting[id]; ok {
		panic("id taken")
	}
	rc := make(chan asyncResult, 1)
	c.awaiting[id] = rc
	c.awaitingMut.Unlock()

	ok := c.send(&Request{
		ID:            id,
		Folder:        folder,
		Name:          name,
		Offset:        offset,
		Size:          int32(size),
		Hash:          hash,
		WeakHash:      weakHash,
		FromTemporary: fromTemporary,
	}, nil)
	if !ok {
		return nil, ErrClosed
	}

	res, ok := <-rc
	if !ok {
		return nil, ErrClosed
	}
	return res.val, res.err
}

// ClusterConfig send the cluster configuration message to the peer and returns any error
func (c *rawConnection) ClusterConfig(config ClusterConfig) {
	c.send(&config, nil)
}

func (c *rawConnection) Closed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

// DownloadProgress sends the progress updates for the files that are currently being downloaded.
func (c *rawConnection) DownloadProgress(folder string, updates []FileDownloadProgressUpdate) {
	c.send(&DownloadProgress{
		Folder:  folder,
		Updates: updates,
	}, nil)
}

func (c *rawConnection) ping() bool {
	return c.send(&Ping{}, nil)
}

func (c *rawConnection) readerLoop() (err error) {
	defer func() {
		c.close(err)
	}()

	fourByteBuf := make([]byte, 4)
	state := stateInitial
	for {
		select {
		case <-c.closed:
			return ErrClosed
		default:
		}

		msg, err := c.readMessage(fourByteBuf)
		if err == errUnknownMessage {
			// Unknown message types are skipped, for future extensibility.
			continue
		}
		if err != nil {
			return err
		}

		switch msg := msg.(type) {
		case *ClusterConfig:
			l.Debugln("read ClusterConfig message")
			if state != stateInitial {
				return fmt.Errorf("protocol error: cluster config message in state %d", state)
			}
			c.receiver.ClusterConfig(c.id, *msg)
			state = stateReady

		case *Index:
			l.Debugln("read Index message")
			if state != stateReady {
				return fmt.Errorf("protocol error: index message in state %d", state)
			}
			if err := checkIndexConsistency(msg.Files); err != nil {
				return fmt.Errorf("protocol error: index: %v", err)
			}
			c.handleIndex(*msg)
			state = stateReady

		case *IndexUpdate:
			l.Debugln("read IndexUpdate message")
			if state != stateReady {
				return fmt.Errorf("protocol error: index update message in state %d", state)
			}
			if err := checkIndexConsistency(msg.Files); err != nil {
				return fmt.Errorf("protocol error: index update: %v", err)
			}
			c.handleIndexUpdate(*msg)
			state = stateReady

		case *Request:
			l.Debugln("read Request message")
			if state != stateReady {
				return fmt.Errorf("protocol error: request message in state %d", state)
			}
			if err := checkFilename(msg.Name); err != nil {
				return fmt.Errorf("protocol error: request: %q: %v", msg.Name, err)
			}
			go c.handleRequest(*msg)

		case *Response:
			l.Debugln("read Response message")
			if state != stateReady {
				return fmt.Errorf("protocol error: response message in state %d", state)
			}
			c.handleResponse(*msg)

		case *DownloadProgress:
			l.Debugln("read DownloadProgress message")
			if state != stateReady {
				return fmt.Errorf("protocol error: response message in state %d", state)
			}
			c.receiver.DownloadProgress(c.id, msg.Folder, msg.Updates)

		case *Ping:
			l.Debugln("read Ping message")
			if state != stateReady {
				return fmt.Errorf("protocol error: ping message in state %d", state)
			}
			// Nothing

		case *Close:
			l.Debugln("read Close message")
			return errors.New(msg.Reason)

		default:
			l.Debugf("read unknown message: %+T", msg)
			return fmt.Errorf("protocol error: %s: unknown or empty message", c.id)
		}
	}
}

func (c *rawConnection) readMessage(fourByteBuf []byte) (message, error) {
	hdr, err := c.readHeader(fourByteBuf)
	if err != nil {
		return nil, err
	}

	return c.readMessageAfterHeader(hdr, fourByteBuf)
}

func (c *rawConnection) readMessageAfterHeader(hdr Header, fourByteBuf []byte) (message, error) {
	// First comes a 4 byte message length

	if _, err := io.ReadFull(c.cr, fourByteBuf[:4]); err != nil {
		return nil, fmt.Errorf("reading message length: %v", err)
	}
	msgLen := int32(binary.BigEndian.Uint32(fourByteBuf))
	if msgLen < 0 {
		return nil, fmt.Errorf("negative message length %d", msgLen)
	}

	// Then comes the message

	buf := BufferPool.Get(int(msgLen))
	if _, err := io.ReadFull(c.cr, buf); err != nil {
		return nil, fmt.Errorf("reading message: %v", err)
	}

	// ... which might be compressed

	switch hdr.Compression {
	case MessageCompressionNone:
		// Nothing

	case MessageCompressionLZ4:
		decomp, err := c.lz4Decompress(buf)
		BufferPool.Put(buf)
		if err != nil {
			return nil, fmt.Errorf("decompressing message: %v", err)
		}
		buf = decomp

	default:
		return nil, fmt.Errorf("unknown message compression %d", hdr.Compression)
	}

	// ... and is then unmarshalled

	msg, err := c.newMessage(hdr.Type)
	if err != nil {
		return nil, err
	}
	if err := msg.Unmarshal(buf); err != nil {
		return nil, fmt.Errorf("unmarshalling message: %v", err)
	}
	BufferPool.Put(buf)

	return msg, nil
}

func (c *rawConnection) readHeader(fourByteBuf []byte) (Header, error) {
	// First comes a 2 byte header length

	if _, err := io.ReadFull(c.cr, fourByteBuf[:2]); err != nil {
		return Header{}, fmt.Errorf("reading length: %v", err)
	}
	hdrLen := int16(binary.BigEndian.Uint16(fourByteBuf))
	if hdrLen < 0 {
		return Header{}, fmt.Errorf("negative header length %d", hdrLen)
	}

	// Then comes the header

	buf := BufferPool.Get(int(hdrLen))
	if _, err := io.ReadFull(c.cr, buf); err != nil {
		return Header{}, fmt.Errorf("reading header: %v", err)
	}

	var hdr Header
	if err := hdr.Unmarshal(buf); err != nil {
		return Header{}, fmt.Errorf("unmarshalling header: %v", err)
	}

	BufferPool.Put(buf)
	return hdr, nil
}

func (c *rawConnection) handleIndex(im Index) {
	l.Debugf("Index(%v, %v, %d file)", c.id, im.Folder, len(im.Files))
	c.receiver.Index(c.id, im.Folder, im.Files)
}

func (c *rawConnection) handleIndexUpdate(im IndexUpdate) {
	l.Debugf("queueing IndexUpdate(%v, %v, %d files)", c.id, im.Folder, len(im.Files))
	c.receiver.IndexUpdate(c.id, im.Folder, im.Files)
}

// checkIndexConsistency verifies a number of invariants on FileInfos received in
// index messages.
func checkIndexConsistency(fs []FileInfo) error {
	for _, f := range fs {
		if err := checkFileInfoConsistency(f); err != nil {
			return fmt.Errorf("%q: %v", f.Name, err)
		}
	}
	return nil
}

// checkFileInfoConsistency verifies a number of invariants on the given FileInfo
func checkFileInfoConsistency(f FileInfo) error {
	if err := checkFilename(f.Name); err != nil {
		return err
	}

	switch {
	case f.Deleted && len(f.Blocks) != 0:
		// Deleted files should have no blocks
		return errDeletedHasBlocks

	case f.Type == FileInfoTypeDirectory && len(f.Blocks) != 0:
		// Directories should have no blocks
		return errDirectoryHasBlocks

	case !f.Deleted && !f.IsInvalid() && f.Type == FileInfoTypeFile && len(f.Blocks) == 0:
		// Non-deleted, non-invalid files should have at least one block
		return errFileHasNoBlocks
	}
	return nil
}

// checkFilename verifies that the given filename is valid according to the
// spec on what's allowed over the wire. A filename failing this test is
// grounds for disconnecting the device.
func checkFilename(name string) error {
	cleanedName := path.Clean(name)
	if cleanedName != name {
		// The filename on the wire should be in canonical format. If
		// Clean() managed to clean it up, there was something wrong with
		// it.
		return errUncleanFilename
	}

	switch name {
	case "", ".", "..":
		// These names are always invalid.
		return errInvalidFilename
	}
	if strings.HasPrefix(name, "/") {
		// Names are folder relative, not absolute.
		return errInvalidFilename
	}
	if strings.HasPrefix(name, "../") {
		// Starting with a dotdot is not allowed. Any other dotdots would
		// have been handled by the Clean() call at the top.
		return errInvalidFilename
	}
	return nil
}

func (c *rawConnection) handleRequest(req Request) {
	res, err := c.receiver.Request(c.id, req.Folder, req.Name, req.Size, req.Offset, req.Hash, req.WeakHash, req.FromTemporary)
	if err != nil {
		c.send(&Response{
			ID:   req.ID,
			Code: errorToCode(err),
		}, nil)
		return
	}
	done := make(chan struct{})
	c.send(&Response{
		ID:   req.ID,
		Data: res.Data(),
		Code: errorToCode(nil),
	}, done)
	<-done
	res.Close()
}

func (c *rawConnection) handleResponse(resp Response) {
	c.awaitingMut.Lock()
	if rc := c.awaiting[resp.ID]; rc != nil {
		delete(c.awaiting, resp.ID)
		rc <- asyncResult{resp.Data, codeToError(resp.Code)}
		close(rc)
	}
	c.awaitingMut.Unlock()
}

func (c *rawConnection) send(msg message, done chan struct{}) bool {
	select {
	case c.outbox <- asyncMessage{msg, done}:
		return true
	case <-c.closed:
		if done != nil {
			close(done)
		}
		return false
	}
}

func (c *rawConnection) writerLoop() {
	for {
		select {
		case hm := <-c.outbox:
			err := c.writeMessage(hm)
			if hm.done != nil {
				close(hm.done)
			}
			if err != nil {
				c.close(err)
				return
			}

		case <-c.closed:
			return
		}
	}
}

func (c *rawConnection) writeMessage(hm asyncMessage) error {
	if c.shouldCompressMessage(hm.msg) {
		return c.writeCompressedMessage(hm)
	}
	return c.writeUncompressedMessage(hm)
}

func (c *rawConnection) writeCompressedMessage(hm asyncMessage) error {
	size := hm.msg.ProtoSize()
	buf := BufferPool.Get(size)
	if _, err := hm.msg.MarshalTo(buf); err != nil {
		return fmt.Errorf("marshalling message: %v", err)
	}

	compressed, err := c.lz4Compress(buf)
	if err != nil {
		return fmt.Errorf("compressing message: %v", err)
	}

	hdr := Header{
		Type:        c.typeOf(hm.msg),
		Compression: MessageCompressionLZ4,
	}
	hdrSize := hdr.ProtoSize()
	if hdrSize > 1<<16-1 {
		panic("impossibly large header")
	}

	totSize := 2 + hdrSize + 4 + len(compressed)
	buf = BufferPool.Upgrade(buf, totSize)

	// Header length
	binary.BigEndian.PutUint16(buf, uint16(hdrSize))
	// Header
	if _, err := hdr.MarshalTo(buf[2:]); err != nil {
		return fmt.Errorf("marshalling header: %v", err)
	}
	// Message length
	binary.BigEndian.PutUint32(buf[2+hdrSize:], uint32(len(compressed)))
	// Message
	copy(buf[2+hdrSize+4:], compressed)
	BufferPool.Put(compressed)

	n, err := c.cw.Write(buf)
	BufferPool.Put(buf)

	l.Debugf("wrote %d bytes on the wire (2 bytes length, %d bytes header, 4 bytes message length, %d bytes message (%d uncompressed)), err=%v", n, hdrSize, len(compressed), size, err)
	if err != nil {
		return fmt.Errorf("writing message: %v", err)
	}
	return nil
}

func (c *rawConnection) writeUncompressedMessage(hm asyncMessage) error {
	size := hm.msg.ProtoSize()

	hdr := Header{
		Type: c.typeOf(hm.msg),
	}
	hdrSize := hdr.ProtoSize()
	if hdrSize > 1<<16-1 {
		panic("impossibly large header")
	}

	totSize := 2 + hdrSize + 4 + size
	buf := BufferPool.Get(totSize)

	// Header length
	binary.BigEndian.PutUint16(buf, uint16(hdrSize))
	// Header
	if _, err := hdr.MarshalTo(buf[2:]); err != nil {
		return fmt.Errorf("marshalling header: %v", err)
	}
	// Message length
	binary.BigEndian.PutUint32(buf[2+hdrSize:], uint32(size))
	// Message
	if _, err := hm.msg.MarshalTo(buf[2+hdrSize+4:]); err != nil {
		return fmt.Errorf("marshalling message: %v", err)
	}

	n, err := c.cw.Write(buf[:totSize])
	BufferPool.Put(buf)

	l.Debugf("wrote %d bytes on the wire (2 bytes length, %d bytes header, 4 bytes message length, %d bytes message), err=%v", n, hdrSize, size, err)
	if err != nil {
		return fmt.Errorf("writing message: %v", err)
	}
	return nil
}

func (c *rawConnection) typeOf(msg message) MessageType {
	switch msg.(type) {
	case *ClusterConfig:
		return messageTypeClusterConfig
	case *Index:
		return messageTypeIndex
	case *IndexUpdate:
		return messageTypeIndexUpdate
	case *Request:
		return messageTypeRequest
	case *Response:
		return messageTypeResponse
	case *DownloadProgress:
		return messageTypeDownloadProgress
	case *Ping:
		return messageTypePing
	case *Close:
		return messageTypeClose
	default:
		panic("bug: unknown message type")
	}
}

func (c *rawConnection) newMessage(t MessageType) (message, error) {
	switch t {
	case messageTypeClusterConfig:
		return new(ClusterConfig), nil
	case messageTypeIndex:
		return new(Index), nil
	case messageTypeIndexUpdate:
		return new(IndexUpdate), nil
	case messageTypeRequest:
		return new(Request), nil
	case messageTypeResponse:
		return new(Response), nil
	case messageTypeDownloadProgress:
		return new(DownloadProgress), nil
	case messageTypePing:
		return new(Ping), nil
	case messageTypeClose:
		return new(Close), nil
	default:
		return nil, errUnknownMessage
	}
}

func (c *rawConnection) shouldCompressMessage(msg message) bool {
	switch c.compression {
	case CompressNever:
		return false

	case CompressAlways:
		// Use compression for large enough messages
		return msg.ProtoSize() >= compressionThreshold

	case CompressMetadata:
		_, isResponse := msg.(*Response)
		// Compress if it's large enough and not a response message
		return !isResponse && msg.ProtoSize() >= compressionThreshold

	default:
		panic("unknown compression setting")
	}
}

func (c *rawConnection) close(err error) {
	c.once.Do(func() {
		l.Debugln("close due to", err)
		close(c.closed)

		c.awaitingMut.Lock()
		for i, ch := range c.awaiting {
			if ch != nil {
				close(ch)
				delete(c.awaiting, i)
			}
		}
		c.awaitingMut.Unlock()

		c.receiver.Closed(c, err)
	})
}

// The pingSender makes sure that we've sent a message within the last
// PingSendInterval. If we already have something sent in the last
// PingSendInterval/2, we do nothing. Otherwise we send a ping message. This
// results in an effecting ping interval of somewhere between
// PingSendInterval/2 and PingSendInterval.
func (c *rawConnection) pingSender() {
	ticker := time.NewTicker(PingSendInterval / 2)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			d := time.Since(c.cw.Last())
			if d < PingSendInterval/2 {
				l.Debugln(c.id, "ping skipped after wr", d)
				continue
			}

			l.Debugln(c.id, "ping -> after", d)
			c.ping()

		case <-c.closed:
			return
		}
	}
}

// The pingReceiver checks that we've received a message (any message will do,
// but we expect pings in the absence of other messages) within the last
// ReceiveTimeout. If not, we close the connection with an ErrTimeout.
func (c *rawConnection) pingReceiver() {
	ticker := time.NewTicker(ReceiveTimeout / 2)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			d := time.Since(c.cr.Last())
			if d > ReceiveTimeout {
				l.Debugln(c.id, "ping timeout", d)
				c.close(ErrTimeout)
			}

			l.Debugln(c.id, "last read within", d)

		case <-c.closed:
			return
		}
	}
}

type Statistics struct {
	At            time.Time
	InBytesTotal  int64
	OutBytesTotal int64
}

func (c *rawConnection) Statistics() Statistics {
	return Statistics{
		At:            time.Now(),
		InBytesTotal:  c.cr.Tot(),
		OutBytesTotal: c.cw.Tot(),
	}
}

func (c *rawConnection) lz4Compress(src []byte) ([]byte, error) {
	var err error
	buf := BufferPool.Get(len(src))
	buf, err = lz4.Encode(buf, src)
	if err != nil {
		return nil, err
	}

	binary.BigEndian.PutUint32(buf, binary.LittleEndian.Uint32(buf))
	return buf, nil
}

func (c *rawConnection) lz4Decompress(src []byte) ([]byte, error) {
	size := binary.BigEndian.Uint32(src)
	binary.LittleEndian.PutUint32(src, size)
	var err error
	buf := BufferPool.Get(int(size))
	buf, err = lz4.Decode(buf, src)
	if err != nil {
		return nil, err
	}
	return buf, nil
}
