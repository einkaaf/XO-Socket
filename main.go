// Tic-Tac-Toe WebSocket Server
//
// Features:
// - Simple matchmaking queue: pairs clients into rooms of 2
// - Turn-based play with server-authoritative validation
// - Broadcasts game state updates to both players
//
// API (WebSocket messages):
// Client -> Server:
//   { "type": "MOVE", "payload": { "position": 0..8 } }
//
// Server -> Client:
//   WAITING: { "type":"WAITING", "payload": { "message": "..." } }
//   START:   { "type":"START", "payload": { "symbol":"X|O", "opponentId":"...", "roomId":"..." } }
//   UPDATE:  { "type":"UPDATE", "payload": { "board":[9]string, "turn":"X|O", "winner":"X|O|DRAW|", "winningLine":[...] } }
//   ERROR:   { "type":"ERROR", "payload": { "message": "..." } }
//   OPPONENT_LEFT: { "type":"OPPONENT_LEFT", "payload": { "message":"..." } }

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 4 * 1024
)

var upgrader = websocket.Upgrader{
	// TODO: tighten this for production (check your frontend origin)
	CheckOrigin: func(r *http.Request) bool { return true },
}

type Message struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// ---- Payload types (avoid interface{} casting) ----

type WaitingPayload struct {
	Message string `json:"message"`
}

type StartPayload struct {
	Symbol     string `json:"symbol"`
	OpponentID string `json:"opponentId"`
	RoomID     string `json:"roomId"`
}

type MovePayload struct {
	Position int `json:"position"`
}

type UpdatePayload struct {
	Board       [9]string `json:"board"`
	Turn        string    `json:"turn"`   // "X" or "O"
	Winner      string    `json:"winner"` // "", "X", "O", "DRAW"
	WinningLine []int     `json:"winningLine,omitempty"`
}

type ErrorPayload struct {
	Message string `json:"message"`
}

type OpponentLeftPayload struct {
	Message string `json:"message"`
}

// ---- Client ----

type Client struct {
	ID   string
	Conn *websocket.Conn
	Send chan any // send concrete payload structs to WriteJSON
	Room *GameRoom

	closeOnce sync.Once
}

func (c *Client) Close() {
	c.closeOnce.Do(func() {
		close(c.Send)
		_ = c.Conn.Close()
	})
}

// ReadPump reads messages  fromthe client and dispatches them.
// It exits on connection errors and triggers unregister.
func (c *Client) ReadPump(m *Manager) {
	defer func() {
		m.Unregister <- c
		c.Close()
	}()

	c.Conn.SetReadLimit(maxMessageSize)
	_ = c.Conn.SetReadDeadline(time.Now().Add(pongWait))
	c.Conn.SetPongHandler(func(string) error {
		_ = c.Conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, data, err := c.Conn.ReadMessage()
		if err != nil {
			return
		}

		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			c.SendError("invalid json message")
			continue
		}

		switch msg.Type {
		case "MOVE":
			if c.Room == nil {
				c.SendError("not in a room")
				continue
			}
			var mv MovePayload
			if err := json.Unmarshal(msg.Payload, &mv); err != nil {
				c.SendError("invalid MOVE payload")
				continue
			}
			if err := c.Room.ApplyMove(c, mv.Position); err != nil {
				c.SendError(err.Error())
			}
		default:
			c.SendError("unknown message type")
		}
	}
}

// WritePump writes messages to the client.
// It also sends periodic pings to keep the connection alive.
func (c *Client) WritePump(ctx context.Context) {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.Close()
	}()

	for {
		select {
		case <-ctx.Done():
			return

		case msg, ok := <-c.Send:
			_ = c.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// channel closed -> close socket
				_ = c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.Conn.WriteJSON(msg); err != nil {
				return
			}

		case <-ticker.C:
			_ = c.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (c *Client) SendError(message string) {
	c.trySend(map[string]any{
		"type":    "ERROR",
		"payload": ErrorPayload{Message: message},
	})
}

func (c *Client) trySend(v any) {
	// Non-blocking best-effort send (avoid deadlocks if client is slow)
	select {
	case c.Send <- v:
	default:
		// drop message if send buffer full
	}
}

// ---- Game Room ----

type GameRoom struct {
	ID      string
	Players [2]*Client
	Board   [9]string

	turnIdx int  // 0 -> X, 1 -> O
	active  bool // false when game ended
	mu      sync.Mutex
}

func NewRoom(id string, p1, p2 *Client) *GameRoom {
	r := &GameRoom{
		ID:      id,
		Players: [2]*Client{p1, p2},
		active:  true,
	}
	p1.Room = r
	p2.Room = r
	return r
}

func (r *GameRoom) SymbolFor(client *Client) (string, int, error) {
	if r.Players[0] == client {
		return "X", 0, nil
	}
	if r.Players[1] == client {
		return "O", 1, nil
	}
	return "", -1, fmt.Errorf("client not in this room")
}

func (r *GameRoom) CurrentTurnSymbol() string {
	if r.turnIdx == 0 {
		return "X"
	}
	return "O"
}

// ApplyMove validates and applies a move server-side, then broadcasts UPDATE.
func (r *GameRoom) ApplyMove(client *Client, pos int) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.active {
		return fmt.Errorf("game is over")
	}

	_, idx, err := r.SymbolFor(client)
	if err != nil {
		return err
	}

	if idx != r.turnIdx {
		return fmt.Errorf("not your turn")
	}

	if pos < 0 || pos > 8 {
		return fmt.Errorf("position must be 0..8")
	}

	if r.Board[pos] != "" {
		return fmt.Errorf("cell already occupied")
	}

	symbol := "X"
	if idx == 1 {
		symbol = "O"
	}

	r.Board[pos] = symbol

	winner, line := checkWinner(r.Board)
	if winner != "" {
		r.active = false
	} else {
		r.turnIdx = 1 - r.turnIdx
	}

	r.Broadcast(map[string]any{
		"type": "UPDATE",
		"payload": UpdatePayload{
			Board:       r.Board,
			Turn:        r.CurrentTurnSymbol(),
			Winner:      winner,
			WinningLine: line,
		},
	})

	return nil
}

// Broadcast sends a message to both players (best-effort).
func (r *GameRoom) Broadcast(msg any) {
	for _, p := range r.Players {
		if p != nil {
			p.trySend(msg)
		}
	}
}

func (r *GameRoom) OpponentOf(c *Client) *Client {
	if r.Players[0] == c {
		return r.Players[1]
	}
	if r.Players[1] == c {
		return r.Players[0]
	}
	return nil
}

func checkWinner(b [9]string) (string, []int) {
	wins := [][]int{
		{0, 1, 2}, {3, 4, 5}, {6, 7, 8},
		{0, 3, 6}, {1, 4, 7}, {2, 5, 8},
		{0, 4, 8}, {2, 4, 6},
	}

	for _, v := range wins {
		if b[v[0]] != "" && b[v[0]] == b[v[1]] && b[v[1]] == b[v[2]] {
			return b[v[0]], v
		}
	}

	// Any empty cell means game continues
	for _, cell := range b {
		if cell == "" {
			return "", nil
		}
	}

	return "DRAW", nil
}

// ---- Matchmaking Manager ----

type Manager struct {
	Clients    map[*Client]bool
	Register   chan *Client
	Unregister chan *Client

	waiting []*Client
	mu      sync.Mutex
}

func NewManager() *Manager {
	return &Manager{
		Clients:    make(map[*Client]bool),
		Register:   make(chan *Client),
		Unregister: make(chan *Client),
		waiting:    make([]*Client, 0),
	}
}

func (m *Manager) Run() {
	for {
		select {
		case c := <-m.Register:
			m.handleRegister(c)

		case c := <-m.Unregister:
			m.handleUnregister(c)
		}
	}
}

func (m *Manager) handleRegister(c *Client) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.Clients[c] = true
	m.waiting = append(m.waiting, c)

	log.Printf("client %s queued (waiting=%d)", c.ID, len(m.waiting))

	if len(m.waiting) < 2 {
		c.trySend(map[string]any{
			"type":    "WAITING",
			"payload": WaitingPayload{Message: "Waiting for an opponent..."},
		})
		return
	}

	p1, p2 := m.waiting[0], m.waiting[1]
	m.waiting = m.waiting[2:]

	room := NewRoom(fmt.Sprintf("room-%d", rand.Intn(1_000_000)), p1, p2)

	// Notify both players of game start + symbols
	p1.trySend(map[string]any{
		"type": "START",
		"payload": StartPayload{
			Symbol:     "X",
			OpponentID: p2.ID,
			RoomID:     room.ID,
		},
	})

	p2.trySend(map[string]any{
		"type": "START",
		"payload": StartPayload{
			Symbol:     "O",
			OpponentID: p1.ID,
			RoomID:     room.ID,
		},
	})

	// Send initial state
	room.Broadcast(map[string]any{
		"type": "UPDATE",
		"payload": UpdatePayload{
			Board:  room.Board,
			Turn:   room.CurrentTurnSymbol(),
			Winner: "",
		},
	})
}

func (m *Manager) handleUnregister(c *Client) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.Clients[c]; !ok {
		return
	}
	delete(m.Clients, c)

	// remove from waiting queue if present
	for i := range m.waiting {
		if m.waiting[i] == c {
			m.waiting = append(m.waiting[:i], m.waiting[i+1:]...)
			break
		}
	}

	// If in a room, notify opponent and detach them
	if c.Room != nil {
		opp := c.Room.OpponentOf(c)
		if opp != nil {
			opp.Room = nil
			opp.trySend(map[string]any{
				"type":    "OPPONENT_LEFT",
				"payload": OpponentLeftPayload{Message: "Opponent disconnected."},
			})
			// Put opponent back into queue automatically (optional)
			m.waiting = append(m.waiting, opp)
		}

		c.Room = nil
	}
}

// ---- main ----

func main() {
	rand.Seed(time.Now().UnixNano())

	mgr := NewManager()
	go mgr.Run()

	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			http.Error(w, "websocket upgrade failed", http.StatusBadRequest)
			return
		}

		client := &Client{
			ID:   fmt.Sprintf("%d", rand.Intn(1_000_000)),
			Conn: conn,
			Send: make(chan any, 16),
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		go client.WritePump(ctx)
		go client.ReadPump(mgr)

		mgr.Register <- client
	})

	log.Println("Tic-Tac-Toe server listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
