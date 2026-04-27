package sdk

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/denisbrodbeck/machineid"
	"github.com/golang-jwt/jwt/v5"
)

const publicKeyPEM = `-----BEGIN PUBLIC KEY-----
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA4q73lqy18mC4fdUpo4rB
viicDRAfKhFLJ15+m7cQ3d5qbjlDfHGHMUxFObRcVB4j+7y67BtJ3BmuAW+0GShW
bUOMSM9q5LtB9es7ocyIznlAIlTgdSqoLcnq5PJbjb2nnAiBiTBuLRlh/DWj5/uD
uNxSrcHQia+vb2WWRogI0W/Nt+7VcB11DpMZndf/9PhuxtEMk82oVibOuJI8mlap
RNDCW5+5rg+AlVvkk4pJefV0uBjJwQmebITzxzT/gW9DtSHUjUVbgke7ig2SOJ0J
/z2JPrw9EPkET1+wOQ0oOQVAiBrNjSUXkP0cW6lCDQor9SideL7iZP7wjsQR30T5
UwIDAQAB
-----END PUBLIC KEY-----`

// BoxConfig obsahuje identifikáciu boxu.
type BoxConfig struct {
	ID      string
	Name    string
	Version string
}

// LicenseClaims obsahuje RS256 JWT claims podľa APISelf v0.5.0.
type LicenseClaims struct {
	LicenseID    string  `json:"lid"`           // UUID licencie
	BoxID        string  `json:"bid"`           // ID boxu (napr. apiself-box-helloworld)
	HWID         string  `json:"hid"`           // HWID stroja (machineid.ProtectedID)
	Email        string  `json:"eml"`           // Email majiteľa
	Plan         string  `json:"pln"`           // Plán: solo | team | unlimited | free | trial
	MaxInstances int     `json:"mxi"`           // Max súčasných inštancií (-1 = neobmedzene)
	Tier         string  `json:"tir,omitempty"` // Feature tier: basic | pro | enterprise | ""
	PricePaid    float64 `json:"prc,omitempty"` // Cena zaplatená v USD pri kúpe (0 = free/trial)
	jwt.RegisteredClaims
}

// GetHWID vráti stabilný HWID tohto stroja.
// Primárne používa machineid.ProtectedID("apiself") — HMAC-SHA256 machine-id,
// s fallback na SHA256(hostname:username) ak machine-id nie je dostupný.
func GetHWID() (string, error) {
	primary, err := machineid.ProtectedID("apiself")
	if err == nil {
		return primary, nil
	}
	hostname, _ := os.Hostname()
	u, _ := user.Current()
	h := sha256.Sum256([]byte(hostname + ":" + u.Username))
	log.Printf("APISelf: machine-id nedostupný, použitý HWID fallback")
	return hex.EncodeToString(h[:]), nil
}

// GetPort vráti port z APISELF_PORT env premennej alebo defaultPort ak nie je nastavená.
func GetPort(defaultPort int) int {
	if v := os.Getenv("APISELF_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			return p
		}
	}
	return defaultPort
}

// GetCoreURL vráti URL Core orchestrátora z APISELF_CORE_URL alebo default localhost:7474.
func GetCoreURL() string {
	if v := os.Getenv("APISELF_CORE_URL"); v != "" {
		return v
	}
	return "http://localhost:7474"
}

// newInstanceID generuje náhodné UUID v4.
func newInstanceID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant bits
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%12x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// registerInstance zaregistruje inštanciu na Cloud serveri.
// Vráti false ak limit bol dosiahnutý alebo server je dostupný a odmietol.
func registerInstance(instanceID, signedToken, cloudURL string) bool {
	body, _ := json.Marshal(map[string]string{
		"signedToken": signedToken,
		"instanceId":  instanceID,
	})
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(cloudURL+"/api/license/instance/register", "application/json", bytes.NewReader(body))
	if err != nil {
		// Cloud nedostupný — povolíme (offline tolerancia)
		log.Printf("APISelf: instance register zlyhal (cloud nedostupný): %v", err)
		return true
	}
	defer resp.Body.Close()

	var result struct {
		Success bool `json:"success"`
		Data    struct {
			Allowed bool `json:"allowed"`
		} `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if !result.Success {
		// Neočakávaná odpoveď → offline tolerancia
		return true
	}
	return result.Data.Allowed
}

// instanceKeepAlive pinguje Cloud server každých 30 minút.
func instanceKeepAlive(instanceID, cloudURL string) {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		body, _ := json.Marshal(map[string]string{"instanceId": instanceID})
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Post(cloudURL+"/api/license/instance/ping", "application/json", bytes.NewReader(body))
		if err == nil {
			resp.Body.Close()
		}
	}
}

// unregisterInstance odhlási inštanciu pri čistom ukončení.
func unregisterInstance(instanceID, cloudURL string) {
	body, _ := json.Marshal(map[string]string{"instanceId": instanceID})
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(cloudURL+"/api/license/instance/unregister", "application/json", bytes.NewReader(body))
	if err == nil {
		resp.Body.Close()
	}
}

// periodicRevocationCheck každých 6 hodín skontroluje platnosť licencie na Cloud serveri.
// Ak je licencia zrušená a Cloud dostupný → zastaví box (os.Exit(1)).
// Ak je Cloud nedostupný → preskočí (offline tolerancia).
func periodicRevocationCheck(conf BoxConfig, token, cloudURL string) {
	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		if !isCloudOnline(cloudURL) {
			log.Printf("APISelf: Cloud nedostupný — revocation check preskočený")
			continue
		}

		body, _ := json.Marshal(map[string]string{"signedToken": token})
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Post(cloudURL+"/api/license/verify", "application/json", bytes.NewReader(body))
		if err != nil {
			continue
		}

		var result struct {
			Success bool `json:"success"`
			Data    struct {
				Valid bool `json:"valid"`
			} `json:"data"`
		}
		json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()

		if result.Success && !result.Data.Valid {
			fmt.Printf("APISelf FATAL: Licencia pre '%s' bola zrušená. Box sa zastaví.\n", conf.ID)
			os.Exit(1)
		}
	}
}

// isCloudOnline vráti true ak je Cloud API server dostupný.
func isCloudOnline(cloudURL string) bool {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(cloudURL + "/api/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// logRW je minimálny ResponseWriter wrapper ktorý zachytí HTTP status kód.
type logRW struct {
	http.ResponseWriter
	status int
}

func (r *logRW) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *logRW) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// isStaticAsset vráti true pre statické assety (JS/CSS/obrázky/fonty) ktoré
// nie sú zaujímavé pre logy ani počítadlo requestov.
func isStaticAsset(path string) bool {
	for _, ext := range []string{".js", ".css", ".map", ".ico", ".png", ".jpg", ".jpeg", ".svg", ".woff", ".woff2", ".ttf", ".eot"} {
		if len(path) > len(ext) && path[len(path)-len(ext):] == ext {
			return true
		}
	}
	return false
}

// ── Logging ───────────────────────────────────────────────────────────────────

// Log je globálny slog logger pre aplikačné udalosti boxu.
// Pred volaním InitLogger smeruje na stdout s text formátom.
// Použitie: sdk.Log.Info("bucket.created", "bucket", name)
var Log *slog.Logger = slog.Default()

var logInitOnce sync.Once

// InitLogger inicializuje štruktúrovaný slog logger pre box.
// Výstup smeruje na stdout (zachytávaný supervisorom → záložka Logs) a do
// rotujúceho súboru logDir/app.log (pre lokálneho vývojára).
//
// boxID je automaticky pridaný do každého záznamu (pole "box_id").
// Formát: JSON (default) alebo text (LOG_FORMAT=text).
// Úroveň: Info (default), laditeľná cez LOG_LEVEL=debug|warn|error.
//
// Použitie:
//
//	sdk.InitLogger("apiself-box-mybox", "")
//	sdk.Log.Info("item.created", "id", item.ID)
func InitLogger(boxID, logDir string) {
	logInitOnce.Do(func() {
		if logDir == "" {
			logDir = os.Getenv("APISELF_LOG_DIR")
		}
		if logDir == "" {
			logDir = "./logs"
		}
		if err := os.MkdirAll(logDir, 0750); err != nil {
			log.Printf("sdk: nemôžem vytvoriť log adresár %s: %v", logDir, err)
		}

		rotWriter := &rotatingWriter{
			path:     filepath.Join(logDir, "app.log"),
			maxBytes: 10 * 1024 * 1024,
			keep:     3,
		}

		level := parseLogLevel(os.Getenv("LOG_LEVEL"))
		w := io.MultiWriter(os.Stdout, rotWriter)
		opts := &slog.HandlerOptions{Level: level}

		var handler slog.Handler
		if strings.ToLower(os.Getenv("LOG_FORMAT")) == "text" {
			handler = slog.NewTextHandler(w, opts)
		} else {
			handler = slog.NewJSONHandler(w, opts)
		}

		Log = slog.New(handler).With("box_id", boxID)
		slog.SetDefault(Log)
	})
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// rotatingWriter zapisuje do súboru a rotuje pri prekročení maxBytes.
// Zachováva keep starých súborov (app.log.1, app.log.2, ...).
type rotatingWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	keep     int
	f        *os.File
	size     int64
}

func (rw *rotatingWriter) Write(p []byte) (int, error) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if rw.f == nil {
		if err := rw.openFile(); err != nil {
			return 0, err
		}
	}
	if rw.size+int64(len(p)) > rw.maxBytes {
		rw.rotate()
	}
	n, err := rw.f.Write(p)
	rw.size += int64(n)
	return n, err
}

func (rw *rotatingWriter) openFile() error {
	if err := os.MkdirAll(filepath.Dir(rw.path), 0750); err != nil {
		return err
	}
	f, err := os.OpenFile(rw.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0640)
	if err != nil {
		return err
	}
	if info, _ := f.Stat(); info != nil {
		rw.size = info.Size()
	}
	rw.f = f
	return nil
}

func (rw *rotatingWriter) rotate() {
	if rw.f != nil {
		rw.f.Close()
		rw.f = nil
	}
	for i := rw.keep - 1; i >= 1; i-- {
		os.Rename(fmt.Sprintf("%s.%d", rw.path, i), fmt.Sprintf("%s.%d", rw.path, i+1)) //nolint:errcheck
	}
	os.Rename(rw.path, rw.path+".1") //nolint:errcheck
	rw.size = 0
	rw.openFile() //nolint:errcheck
}

// LoggingMiddleware obalí HTTP handler a každý request zaznamená v APISelf Manageri.
//
// Requesty prichádzajúce cez manager proxy (X-Forwarded-By: apiself-core) sú preskočené —
// proxy ich loguje sama. Priame requesty (direct) sú odoslané na POST /api/core/log.
// Statické assety (JS, CSS, obrázky) sú ticho preskočené — nie sú zaujímavé pre logy.
//
// Použitie v boxe:
//
//	http.ListenAndServe(addr, sdk.LoggingMiddleware(boxID, mux))
func LoggingMiddleware(boxID string, next http.Handler) http.Handler {
	coreURL := GetCoreURL()
	client := &http.Client{Timeout: 2 * time.Second}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Requesty cez proxy sú už zalogované proxy vrstvou — preskočíme
		if r.Header.Get("X-Forwarded-By") == "apiself-core" {
			next.ServeHTTP(w, r)
			return
		}

		// Statické assety nelogujeme — zbytočný šum
		if isStaticAsset(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		rec := &logRW{ResponseWriter: w, status: 0}
		start := time.Now()
		next.ServeHTTP(rec, r)
		elapsed := time.Since(start).Milliseconds()

		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}

		// Extrahuj IP adresu klienta
		ip := r.RemoteAddr
		if host, _, err := net.SplitHostPort(ip); err == nil {
			ip = host
		}

		msg := fmt.Sprintf("%s %s → %d (%dms) [direct, from: %s]", r.Method, r.URL.Path, status, elapsed, ip)

		go func() {
			payload, _ := json.Marshal(map[string]string{
				"boxId":   boxID,
				"stream":  "request",
				"message": msg,
			})
			resp, err := client.Post(coreURL+"/api/core/log", "application/json", bytes.NewReader(payload))
			if err == nil {
				resp.Body.Close()
			}
		}()
	})
}
