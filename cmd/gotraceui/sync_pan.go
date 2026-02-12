package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"honnef.co/go/gotraceui/layout"
	"honnef.co/go/gotraceui/theme"

	exptrace "golang.org/x/exp/trace"
)

const panSyncPeersScanInterval = 750 * time.Millisecond

type panSyncInstanceFile struct {
	ID      string `json:"id"`
	PID     int    `json:"pid"`
	UDPPort int    `json:"udp_port"`
}

type panSyncMessage struct {
	Sender string  `json:"sender"`
	DStart int64   `json:"d_start"`
	DY     float64 `json:"d_y"`
}

type panSyncPeer struct {
	id   string
	port int
}

type panSyncService struct {
	id string

	conn *net.UDPConn

	regDir  string
	regFile string

	mu            sync.Mutex
	peers         []panSyncPeer
	peersLastScan time.Time

	last struct {
		set   bool
		start exptrace.Time
		y     normalizedY
	}

	stop chan struct{}
}

func newPanSyncService() (*panSyncService, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(b[:])
	regDir := filepath.Join(os.TempDir(), "gotraceui-sync-pan")
	return &panSyncService{
		id:     id,
		regDir: regDir,
		stop:   make(chan struct{}),
	}, nil
}

func (s *panSyncService) Start(onDelta func(dStart exptrace.Time, dY normalizedY), invalidate func()) error {
	if err := os.MkdirAll(s.regDir, 0o755); err != nil {
		return err
	}

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		return err
	}
	s.conn = conn

	port := conn.LocalAddr().(*net.UDPAddr).Port
	s.regFile = filepath.Join(s.regDir, fmt.Sprintf("%s.json", s.id))
	inst := panSyncInstanceFile{
		ID:      s.id,
		PID:     os.Getpid(),
		UDPPort: port,
	}
	b, err := json.Marshal(inst)
	if err != nil {
		conn.Close()
		return err
	}
	if err := os.WriteFile(s.regFile, b, 0o644); err != nil {
		conn.Close()
		return err
	}

	go s.recvLoop(onDelta, invalidate)
	return nil
}

func (s *panSyncService) Stop() {
	select {
	case <-s.stop:
		// already stopped
		return
	default:
	}
	close(s.stop)
	if s.conn != nil {
		_ = s.conn.Close()
	}
	if s.regFile != "" {
		_ = os.Remove(s.regFile)
	}
}

func (s *panSyncService) SetBaseline(start exptrace.Time, y normalizedY) {
	s.last.set = true
	s.last.start = start
	s.last.y = y
}

func (s *panSyncService) EnsureBaseline(start exptrace.Time, y normalizedY) {
	if s.last.set {
		return
	}
	s.last.set = true
	s.last.start = start
	s.last.y = y
}

func (s *panSyncService) Broadcast(start exptrace.Time, y normalizedY) {
	if s.conn == nil {
		return
	}

	if !s.last.set {
		// Shouldn't happen in practice; SetBaseline is called when enabling the feature.
		s.last.set = true
		s.last.start = start
		s.last.y = y
		return
	}

	ds := start - s.last.start
	dy := y - s.last.y
	if ds == 0 {
		return
	}

	s.last.start = start
	s.last.y = y

	msg := panSyncMessage{Sender: s.id, DStart: int64(ds), DY: float64(dy)}
	b, err := json.Marshal(msg)
	if err != nil {
		return
	}

	peers := s.getPeers()
	for _, p := range peers {
		if p.id == s.id {
			continue
		}
		_, _ = s.conn.WriteToUDP(b, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: p.port})
	}
}

func (s *panSyncService) getPeers() []panSyncPeer {
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	if now.Sub(s.peersLastScan) < panSyncPeersScanInterval && s.peers != nil {
		return append([]panSyncPeer(nil), s.peers...)
	}
	s.peersLastScan = now

	ents, err := os.ReadDir(s.regDir)
	if err != nil {
		s.peers = nil
		return nil
	}

	peers := s.peers[:0]
	for _, ent := range ents {
		if ent.IsDir() {
			continue
		}
		// Keep this deliberately lax; it's a best-effort feature.
		path := filepath.Join(s.regDir, ent.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var inst panSyncInstanceFile
		if err := json.Unmarshal(b, &inst); err != nil {
			continue
		}
		if inst.UDPPort <= 0 || inst.UDPPort > 65535 || inst.ID == "" {
			continue
		}
		peers = append(peers, panSyncPeer{id: inst.ID, port: inst.UDPPort})
	}
	s.peers = peers
	return append([]panSyncPeer(nil), s.peers...)
}

func (s *panSyncService) recvLoop(onDelta func(dStart exptrace.Time, dY normalizedY), invalidate func()) {
	buf := make([]byte, 1024)
	for {
		_ = s.conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
		n, _, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				select {
				case <-s.stop:
					return
				default:
					continue
				}
			}
			select {
			case <-s.stop:
				return
			default:
				// socket error; bail out quietly
				return
			}
		}

		var msg panSyncMessage
		if err := json.Unmarshal(buf[:n], &msg); err != nil {
			continue
		}
		if msg.Sender == "" || msg.Sender == s.id {
			continue
		}

		onDelta(exptrace.Time(msg.DStart), normalizedY(msg.DY))
		invalidate()
	}
}

func (mwin *MainWindow) setPanSyncEnabled(win *theme.Window, gtx layout.Context, enabled bool) {
	mwin.syncPanAllTraces = enabled
	if !enabled {
		if mwin.panSync != nil {
			mwin.panSync.Stop()
			mwin.panSync = nil
		}
		win.ShowNotification(gtx, "Sync pan disabled")
		return
	}

	if mwin.panSync != nil {
		mwin.panSync.SetBaseline(mwin.canvas.start, mwin.canvas.y)
		return
	}

	svc, err := newPanSyncService()
	if err != nil {
		win.ShowNotification(gtx, fmt.Sprintf("Couldn't enable sync pan: %s", err))
		mwin.syncPanAllTraces = false
		return
	}

	if mwin.canvas.nsPerPx != 0 {
		svc.SetBaseline(mwin.canvas.start, mwin.canvas.y)
	}

	onDelta := func(dStart exptrace.Time, _ normalizedY) {
		mwin.twin.EmitAction(theme.ExecuteAction(func(gtx layout.Context) {
			mwin.canvas.applyHorizontalDeltaFromSync(gtx, dStart)
		}))
	}
	invalidate := func() {
		mwin.twin.AppWindow.Invalidate()
	}

	if err := svc.Start(onDelta, invalidate); err != nil {
		win.ShowNotification(gtx, fmt.Sprintf("Couldn't enable sync pan: %s", err))
		mwin.syncPanAllTraces = false
		return
	}
	mwin.panSync = svc
	win.ShowNotification(gtx, "Sync pan enabled")
}
