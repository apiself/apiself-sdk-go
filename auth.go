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

// GetUser extrahuje user-a z hlavičiek injektovaných Manager proxy.
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
//
// Bezpečnosť: tieto hlavičky stripuje Manager proxy z client requestov
// pred ich prepisom (pozri internal/proxy/proxy.go) — box im môže veriť
// keďže jediná cesta dovnútra je cez Manager.
func GetUser(r *http.Request) User {
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

// RequireManagerProxy je HTTP middleware ktorý zamietne request s 401 ak
// nepricel cez Manager proxy. Použi ho v multi-user mode-e ako defense-in-depth
// proti direct-port-access bypass-u.
//
// V single-user mode (žiadny auth box detekovaný Manager-om) middleware
// pass-throughne — direct access je vtedy povolený, lebo placeholder
// owner má rovnaké práva ako Manager session.
//
// Použitie:
//
//	mux.Handle("/api/admin/", sdk.RequireManagerProxy(http.HandlerFunc(h.AdminHandler)))
//
// Alebo jednoduchšie cez wrapper-r:
//
//	apiMux := http.NewServeMux()
//	apiMux.HandleFunc("/api/forms", h.ListForms)
//	mux.Handle("/api/forms", sdk.RequireManagerProxy(apiMux))
func RequireManagerProxy(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Single-user mode (žiadny auth box) → pass-through. Phase 1 mandatory
		// password gating Manager-a chráni pred unauthorized install / config.
		if !IsMultiUser() {
			next.ServeHTTP(w, r)
			return
		}
		if !IsManagerProxied(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"success":false,"error":"This box must be accessed through APISelf Manager. Direct port access is blocked in multi-user mode.","code":"auth.proxy_required"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}
