package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	_ "github.com/lib/pq"
	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

/* =========================
   CONFIGURATION & MODELS
========================= */

type Config struct {
	Server struct {
		Port      string `yaml:"port"`
		JWTSecret string `yaml:"jwt_secret"`
	} `yaml:"server"`
	Database struct {
		Host     string `yaml:"host"`
		Port     int    `yaml:"port"`
		User     string `yaml:"user"`
		Password string `yaml:"password"`
		DBName   string `yaml:"dbname"`
		SSLMode  string `yaml:"sslmode"`
	} `yaml:"database"`
}

var (
	cfg      Config
	db       *sql.DB
	upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
)

type User struct {
	ID         uuid.UUID `json:"id"`
	Username   string    `json:"username"`
	Wins       int       `json:"wins"`
	Losses     int       `json:"losses"`
	Draws      int       `json:"draws"`
	TotalScore int       `json:"total_score"`
}

type Client struct {
	UserID uuid.UUID
	Conn   *websocket.Conn
	Send   chan []byte
	Game   *Game
}

type Game struct {
	ID    uuid.UUID
	P1    *Client   // Player X
	P2    *Client   // Player O
	Board [9]string // "", "X", "O"
	Turn  uuid.UUID
	mu    sync.Mutex
}

/* =========================
   HUB & MATCHMAKING
========================= */

type Hub struct {
	clients map[uuid.UUID]*Client
	queue   chan *Client
	mu      sync.Mutex
}

var hub = Hub{
	clients: make(map[uuid.UUID]*Client),
	queue:   make(chan *Client, 100),
}

func (h *Hub) Matchmaker() {
	for {
		p1 := <-h.queue
		p2 := <-h.queue
		go startGame(p1, p2)
	}
}

/* =========================
   AUTH UTILS
========================= */

type Claims struct {
	UserID uuid.UUID `json:"user_id"`
	jwt.RegisteredClaims
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	var req struct{ Username, Password string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad request", 400)
		return
	}

	var u User
	var hash string
	err := db.QueryRow("SELECT id, password FROM users WHERE username=$1", req.Username).Scan(&u.ID, &hash)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) != nil {
		http.Error(w, "Invalid credentials", 401)
		return
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		UserID:           u.ID,
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour))},
	})
	tString, _ := token.SignedString([]byte(cfg.Server.JWTSecret))
	json.NewEncoder(w).Encode(map[string]string{"token": tString})
}

/* =========================
   WEBSOCKET CORE
========================= */

func wsHandler(w http.ResponseWriter, r *http.Request) {
	tokenStr := r.URL.Query().Get("token")
	token, err := jwt.ParseWithClaims(tokenStr, &Claims{}, func(t *jwt.Token) (interface{}, error) {
		return []byte(cfg.Server.JWTSecret), nil
	})

	if err != nil || !token.Valid {
		http.Error(w, "Unauthorized", 401)
		return
	}
	uid := token.Claims.(*Claims).UserID

	conn, _ := upgrader.Upgrade(w, r, nil)
	client := &Client{UserID: uid, Conn: conn, Send: make(chan []byte, 256)}

	hub.mu.Lock()
	hub.clients[uid] = client
	hub.mu.Unlock()

	db.Exec("UPDATE users SET status='online' WHERE id=$1", uid)

	go client.writePump()
	go client.readPump()
}

func (c *Client) readPump() {
	defer func() {
		hub.mu.Lock()
		delete(hub.clients, c.UserID)
		hub.mu.Unlock()
		db.Exec("UPDATE users SET status='offline' WHERE id=$1", c.UserID)
		c.Conn.Close()
	}()

	c.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.Conn.SetPongHandler(func(string) error {
		c.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		_, message, err := c.Conn.ReadMessage()
		if err != nil {
			break
		}
		var msg struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
		}
		json.Unmarshal(message, &msg)

		switch msg.Type {
		case "join_queue":
			hub.queue <- c
		case "move":
			if c.Game != nil {
				c.Game.Play(c.UserID, msg.Index)
			}
		}
	}
}

func (c *Client) writePump() {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case msg, ok := <-c.Send:
			if !ok {
				c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			c.Conn.WriteMessage(websocket.TextMessage, msg)
		case <-ticker.C:
			if err := c.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

/* =========================
   GAME LOGIC
========================= */

func startGame(p1, p2 *Client) {
	g := &Game{ID: uuid.New(), P1: p1, P2: p2, Turn: p1.UserID}
	p1.Game = g
	p2.Game = g
	db.Exec("UPDATE users SET status='playing' WHERE id IN ($1, $2)", p1.UserID, p2.UserID)
	g.broadcast(map[string]interface{}{"type": "game_start", "turn": g.Turn, "game_id": g.ID})
}

func (g *Game) Play(uid uuid.UUID, idx int) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.Turn != uid || idx < 0 || idx > 8 || g.Board[idx] != "" {
		return
	}

	mark := "X"
	if uid == g.P2.UserID {
		mark = "O"
	}
	g.Board[idx] = mark

	if winner := g.checkWinner(); winner != "" {
		g.endGame(winner)
	} else if g.isDraw() {
		g.endGame("draw")
	} else {
		if g.Turn == g.P1.UserID {
			g.Turn = g.P2.UserID
		} else {
			g.Turn = g.P1.UserID
		}
		g.broadcast(map[string]interface{}{"type": "update", "board": g.Board, "turn": g.Turn})
	}
}

func (g *Game) checkWinner() string {
	winPatterns := [8][3]int{{0, 1, 2}, {3, 4, 5}, {6, 7, 8}, {0, 3, 6}, {1, 4, 7}, {2, 5, 8}, {0, 4, 8}, {2, 4, 6}}
	for _, p := range winPatterns {
		if g.Board[p[0]] != "" && g.Board[p[0]] == g.Board[p[1]] && g.Board[p[0]] == g.Board[p[2]] {
			return g.Board[p[0]]
		}
	}
	return ""
}

func (g *Game) isDraw() bool {
	for _, v := range g.Board {
		if v == "" {
			return false
		}
	}
	return true
}

func (g *Game) endGame(result string) {
	g.broadcast(map[string]interface{}{"type": "game_over", "result": result, "board": g.Board})

	if result == "draw" {
		db.Exec("UPDATE users SET draws=draws+1, total_games=total_games+1, total_score=total_score+1 WHERE id IN ($1, $2)", g.P1.UserID, g.P2.UserID)
	} else {
		winner, loser := g.P1.UserID, g.P2.UserID
		if result == "O" {
			winner, loser = g.P2.UserID, g.P1.UserID
		}
		db.Exec("UPDATE users SET wins=wins+1, total_games=total_games+1, total_score=total_score+3 WHERE id=$1", winner)
		db.Exec("UPDATE users SET losses=losses+1, total_games=total_games+1 WHERE id=$1", loser)
	}
	db.Exec("UPDATE users SET status='online' WHERE id IN ($1, $2)", g.P1.UserID, g.P2.UserID)
	g.P1.Game = nil
	g.P2.Game = nil
}

func (g *Game) broadcast(val interface{}) {
	b, _ := json.Marshal(val)
	g.P1.Send <- b
	g.P2.Send <- b
}

/* =========================
   INITIALIZATION
========================= */

func main() {
	// 1. Load Config
	configFile, _ := os.Open("config.yaml")
	yaml.NewDecoder(configFile).Decode(&cfg)

	// 2. Database Connect
	dsn := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		cfg.Database.Host, cfg.Database.Port, cfg.Database.User, cfg.Database.Password, cfg.Database.DBName, cfg.Database.SSLMode)
	var err error
	db, err = sql.Open("postgres", dsn)
	if err != nil {
		log.Fatal(err)
	}

	// 3. Start Hub
	go hub.Matchmaker()

	// 4. Routes
	http.HandleFunc("/login", loginHandler)
	http.HandleFunc("/ws", wsHandler)

	fmt.Printf("XO Backend live on port %s\n", cfg.Server.Port)
	log.Fatal(http.ListenAndServe(":"+cfg.Server.Port, nil))
}
