package sdk

import (
	"net/http"
	"os"
)

// User reprezentuje aktuálneho prihláseného používateľa.
//
// Phase 1 (single-user mode): keď nie je nainštalovaný auth box, Manager
// injektuje placeholder hlavičky `X-APISelf-User: owner`,
// `X-APISelf-Tier: admin`, `X-APISelf-Admin: 1`. GetUser vráti owner-a
// bez ďalšieho fetch-u — box code nemusí rozlišovať.
//
// Phase 2 (multi-user mode, auth box installed): Manager validuje
// `apiself_auth` cookie cez auth box public key (cache-ovaný v Manager DB)
// a injektuje reálneho user-a do hlavičiek pri každom proxy requeste.
type User struct {
	// ID — stabilný identifikátor user-a. "owner" v single-user móde,
	// jinak UUID z auth box DB.
	ID string

	// Email — voliteľný (môže byť prázdny v single-user móde).
	Email string

	// Role — "admin" / "member" / "viewer" v multi-user, "admin" pre owner-a
	// v single-user. Box code zriedka číta priamo — preferuj IsAdmin / HasRole.
	Role string

	// IsAdmin — true ak Manager nahodil X-APISelf-Admin=1 (owner ALEBO
	// auth box rola admin/owner).
	IsAdmin bool

	// IsOwner — true ak Manager identifikuje user-a ako Manager owner-a
	// (single-user mode placeholder, alebo prvý user v auth box DB).
	IsOwner bool

	// Authenticated — true ak X-APISelf-User hlavička je nastavená.
	// V Phase 1 je vždy true (Manager injectuje placeholder owner). V
	// Phase 2 je false ak auth box session cookie chýba alebo je expirovaná
	// — box by potom nemal renderovať user-specific content (use AuthGuard
	// v UI alebo RequireAuth middleware na strane servera).
	Authenticated bool
}

// authBoxID je ID hostiteľského boxu pre účely auth pubkey cache.
// Nastavuje sa cez SetAuthBoxID() pri InitBox(); pre testy a edge cases má
// rozumný default ("apiself-box-unknown") aby ValidateJWT nepadalo na panic.
var authBoxID = "apiself-box-unknown"

// SetAuthBoxID registruje hostiteľský box ID pre auth pubkey cache.
// InitBox to volá automaticky; box code ho explicitne volať nepotrebuje.
func SetAuthBoxID(id string) {
	if id != "" {
		authBoxID = id
	}
}

// GetUser extrahuje user-a v tomto poradí:
//
//  1. Manager-proxied request (X-Forwarded-By: apiself-core) → trust headers.
//     Toto je rýchla cesta — manager už validoval cookie a injektol hlavičky,
//     žiadny dôvod robiť to znova.
//  2. Inak (direct-port access) → extract JWT z cookie alebo Authorization
//     header a validuj lokálne pomocou cached auth-box pubkey-u.
//  3. Bez tokenu / invalid token → User{Authenticated: false}.
//
// Box code typicky používa:
//
//	user := sdk.GetUser(r)
//	if user.IsAdmin {
//	    // admin-only logic
//	}
//	row.OwnerID = user.ID  // tag DB row's owner
//
// Pre kontroly typu "stačí member alebo vyšší" preferuj sdk.HasRole(r, "member").
func GetUser(r *http.Request) User {
	// Cesta 1 — manager-proxied. X-Forwarded-By je nastavená iba managerom
	// po stripnutí všetkých client-supplied X-APISelf-* hlavičiek, takže
	// im môžeme veriť. Bez tejto hlavičky by sa dali zaspoofiť.
	if r.Header.Get("X-Forwarded-By") == "apiself-core" {
		id := r.Header.Get("X-APISelf-User")
		return User{
			ID:            id,
			Email:         r.Header.Get("X-APISelf-Email"),
			Role:          r.Header.Get("X-APISelf-Tier"),
			IsAdmin:       r.Header.Get("X-APISelf-Admin") == "1",
			IsOwner:       id == "owner" || r.Header.Get("X-APISelf-Owner") == "1",
			Authenticated: id != "",
		}
	}

	// Cesta 2 — direct port. Extract token z cookie / Authorization header
	// a validuj cez cached pubkey. Funguje aj keď manager nebeží — box je
	// sebestačný.
	token := extractToken(r)
	if token == "" {
		return User{Authenticated: false}
	}
	claims, err := ValidateJWT(authBoxID, token)
	if err != nil || claims == nil {
		return User{Authenticated: false}
	}
	return userFromClaims(claims)
}

// roleRank mapuje rolu na číselnú hodnotu pre porovnávanie.
// Čím vyššie tým viac práv: viewer < member < admin.
func roleRank(role string) int {
	switch role {
	case "admin":
		return 3
	case "member":
		return 2
	case "viewer":
		return 1
	default:
		return 0
	}
}

// HasRole vráti true ak user má aspoň danú rolu (admin >= member >= viewer).
//
//	if !sdk.HasRole(r, "member") {
//	    http.Error(w, "viewer cannot create", http.StatusForbidden)
//	    return
//	}
func HasRole(r *http.Request, minRole string) bool {
	user := GetUser(r)
	if user.IsAdmin || user.IsOwner {
		return true
	}
	return roleRank(user.Role) >= roleRank(minRole)
}

// RequireRole je HTTP middleware ktorý vracia 403 Forbidden ak user nemá
// minimálnu rolu. Skladá sa s ostatnými handlermi:
//
//	mux.Handle("/api/admin/users", sdk.RequireRole("admin", h.AdminListUsers))
//
// V single-user móde (Phase 1) prejde vždy lebo placeholder owner má IsAdmin=true.
func RequireRole(minRole string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !HasRole(r, minRole) {
			http.Error(w, `{"success":false,"error":"forbidden","code":"auth.role_insufficient"}`, http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// IsMultiUser vráti true ak Manager detekoval bežiaci auth box a injektoval
// `APISELF_AUTH_BOX_URL` env var pri štarte boxu.
//
// Box code môže pivotovať UI:
//
//	if sdk.IsMultiUser() {
//	    // ukáž "share with team" tlačidlo
//	} else {
//	    // single-user — schovaj pozvánky / RBAC sekciu
//	}
//
// Pre per-feature gating preferuj per-row owner_id check + HasRole — toto
// je iba globálny mode signál.
func IsMultiUser() bool {
	return os.Getenv("APISELF_AUTH_BOX_URL") != ""
}

// AuthBoxURL vráti URL na auth box (alebo "" ak nie je nainštalovaný).
// Použité hlavne SDK UI komponentmi pre fetch /api/whoami priamo cez
// proxy URL — server-side Go kód obvykle vystačí s GetUser(r).
func AuthBoxURL() string {
	return os.Getenv("APISELF_AUTH_BOX_URL")
}

// IsManagerProxied vráti true ak request prišiel cez Manager proxy
// (X-Forwarded-By=apiself-core hlavička, ktorú Manager bezpečne nastavuje
// po stripnutí všetkých client-supplied X-* hlavičiek).
//
// Direct-port access (napr. cez localhost:7610) NEMÁ túto hlavičku —
// nikto okrem Manager-a ju nemôže nahodiť.
func IsManagerProxied(r *http.Request) bool {
	return r.Header.Get("X-Forwarded-By") == "apiself-core"
}

// RequireAuth je HTTP middleware ktorý vráti 401 ak request nie je
// autentifikovaný. Akceptuje OBE cesty (v0.7):
//
//   - Manager-proxied request s X-APISelf-User hlavičkou
//   - Direct-port request s validným cookie / Authorization Bearer JWT
//
// V single-user mode (auth box nebeží alebo nie je nainštalovaný)
// middleware pass-throughne — placeholder owner je vždy "prihlásený".
//
// Použitie:
//
//	mux.Handle("/api/admin/", sdk.RequireAuth(http.HandlerFunc(h.AdminHandler)))
func RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Single-user mode → pass-through. APISELF_AUTH_BOX_URL chýba
		// znamená že auth box nikdy nebol detekovaný managerom.
		if !IsMultiUser() {
			next.ServeHTTP(w, r)
			return
		}
		u := GetUser(r)
		if !u.Authenticated {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"success":false,"error":"authentication required","code":"auth.required"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireManagerProxy je deprecated alias na RequireAuth. Ponechané pre
// backward compat boxov ktoré ho používali pred v0.7. Nové boxy nech
// volajú RequireAuth priamo.
//
// Pôvodné správanie (tvrdo blokovať direct port v multi-user mode) bolo v0.7
// uvoľnené: box vie teraz validovať JWT sám, takže direct port je legitímna
// cesta pokým má valid token.
//
// Deprecated: použi RequireAuth.
func RequireManagerProxy(next http.Handler) http.Handler {
	return RequireAuth(next)
}
