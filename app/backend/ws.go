// ws.go — WebSocket real-time push
//
// Architecture: hub (the center) + several client goroutines
// When backend state changes → hub.broadcast → every connected Web UI refreshes live

package main

import (
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

// ── Upgrader ──────────────────────────────────────────────────────────

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true }, // localhost only
}

// ── Hub ───────────────────────────────────────────────────────────────

type wsHub struct {
	mu      sync.RWMutex
	clients map[*wsClient]bool
}

type wsClient struct {
	conn *websocket.Conn
	send chan []byte
}

func newWSHub() *wsHub {
	return &wsHub{
		clients: make(map[*wsClient]bool),
	}
}

func (h *wsHub) run() {
	// the hub stays alive; it has nothing to do on its own
	select {}
}

func (h *wsHub) add(c *wsClient) {
	h.mu.Lock()
	h.clients[c] = true
	h.mu.Unlock()
	log.Printf("WS client connected (%d total)", len(h.clients))
}

func (h *wsHub) remove(c *wsClient) {
	h.mu.Lock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		close(c.send)
	}
	h.mu.Unlock()
	log.Printf("WS client disconnected (%d total)", len(h.clients))
}

func (h *wsHub) broadcast(msg []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		select {
		case c.send <- msg:
		default:
			// client channel full → drop (avoids blocking)
		}
	}
}

// ── WebSocket Handler ─────────────────────────────────────────────────

func serveWS(hub *wsHub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WS upgrade: %v", err)
		return
	}

	client := &wsClient{
		conn: conn,
		send: make(chan []byte, 16),
	}
	hub.add(client)

	// writer goroutine: push messages from the send channel to the client
	go func() {
		defer conn.Close()
		for msg := range client.send {
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				log.Printf("WS write: %v", err)
				break
			}
		}
	}()

	// reader goroutine: receive client messages (not needed right now, but keeps the connection alive)
	go func() {
		defer hub.remove(client)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				break
			}
		}
	}()
}
