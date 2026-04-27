package sdk

// MeshClient umožňuje boxom komunikovať cez APISelf P2P mesh sieť.
// Správy sú prenášané managerom (Noise protokol) — box nemusí poznať sieťovú topológiu.
// Box posiela správy cez POST /api/mesh/send na localhoste,
// a prijíma ich cez GET /api/mesh/inbox/{boxId} (store-and-forward).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// MeshClient je klient pre P2P mesh komunikáciu cez APISelf Manager.
type MeshClient struct {
	boxID      string
	coreURL    string
	httpClient *http.Client
}

// NewMeshClient vytvorí nového MeshClient pre daný box.
// boxID: ID boxu (napr. "apiself-box-chat")
// coreURL: URL managera (default: z APISELF_CORE_URL alebo "http://localhost:7474")
func NewMeshClient(boxID string) *MeshClient {
	return &MeshClient{
		boxID:      boxID,
		coreURL:    GetCoreURL(),
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// MeshMessage je správa prijatá z mesh siete.
type MeshMessage struct {
	SrcHWID string          `json:"srcHWID"` // HWID odosielateľa
	Payload json.RawMessage `json:"payload"` // obsah správy
	At      time.Time       `json:"at"`      // čas prijatia
}

// Send odošle správu vzdialenému managerovi (identifikovaný HWID).
// dstHWID: cieľový HWID managera
// payload: ľubovoľná JSON-serializovateľná hodnota
// Správa bude doručená do inboxu boxu s rovnakým boxID na cieľovom manageri.
func (c *MeshClient) Send(dstHWID string, payload interface{}) error {
	payloadRaw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("mesh send: serializácia payload: %w", err)
	}
	body, _ := json.Marshal(map[string]interface{}{
		"dstHWID": dstHWID,
		"dstBox":  c.boxID,
		"payload": json.RawMessage(payloadRaw),
	})
	resp, err := c.httpClient.Post(
		c.coreURL+"/api/mesh/send",
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		return fmt.Errorf("mesh send: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("mesh send: HTTP %d: %s", resp.StatusCode, string(raw))
	}
	return nil
}

// Poll vyzdvihne a vráti správy čakajúce v inboxe tohto boxu.
// Inbox je vyprázdnený po každom Poll() — správy sú doručené práve raz.
// Vráti prázdny slice ak nie sú žiadne správy.
func (c *MeshClient) Poll() ([]MeshMessage, error) {
	resp, err := c.httpClient.Get(
		c.coreURL + "/api/mesh/inbox/" + c.boxID,
	)
	if err != nil {
		return nil, fmt.Errorf("mesh poll: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var apiResp struct {
		Success bool          `json:"success"`
		Data    []MeshMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &apiResp); err != nil || !apiResp.Success {
		return nil, fmt.Errorf("mesh poll: neplatná odpoveď")
	}
	if apiResp.Data == nil {
		return []MeshMessage{}, nil
	}
	return apiResp.Data, nil
}

// Subscribe spustí polling v pozadí a volá callback pre každú správu.
// interval: ako často sa polluje (napr. 2*time.Second)
// Vráti cancel funkciu na zastavenie.
func (c *MeshClient) Subscribe(interval time.Duration, fn func(MeshMessage)) func() {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				msgs, err := c.Poll()
				if err != nil {
					continue
				}
				for _, msg := range msgs {
					fn(msg)
				}
			}
		}
	}()
	return func() { close(done) }
}

// MeshInfo vráti informácie o lokálnom P2P node (pubkey, peers, atď.).
func (c *MeshClient) MeshInfo() (map[string]interface{}, error) {
	resp, err := c.httpClient.Get(c.coreURL + "/api/mesh/info")
	if err != nil {
		return nil, fmt.Errorf("mesh info: %w", err)
	}
	defer resp.Body.Close()
	var result struct {
		Success bool                   `json:"success"`
		Data    map[string]interface{} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || !result.Success {
		return nil, fmt.Errorf("mesh info: neplatná odpoveď")
	}
	return result.Data, nil
}
