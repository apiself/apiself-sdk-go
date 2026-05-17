package sdk

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

// SessionClaims — to čo auth box podpisuje do session JWT.
// Mirror SessionClaims z apiself-box-auth/internal/auth/jwt.go.
// Box nepotrebuje celý struct — len polia ktoré GetUser číta.
type SessionClaims struct {
	jwt.RegisteredClaims
	UserID    string `json:"uid"`
	Email     string `json:"eml"`
	Role      string `json:"rol"`
	IsOwner   bool   `json:"own,omitempty"`
	SessionID string `json:"sid"`
}

// ValidateJWT parse-uje a verifikuje auth-box-issued session JWT pomocou
// lokálne cached RSA pubkey-u (alebo lazy-fetchnutého ak ešte nie je).
//
// `boxID` je hostiteľský box, jeho data dir je miesto kde sa pubkey
// cache-uje na disk (najčastejšie len jeden raz).
//
// Acceptance criteria:
//
//   - signing method = RS256
//   - issuer = "apiself-box-auth"
//   - not expired (RegisteredClaims.ExpiresAt)
//   - signature verifies against cached pubkey
//
// Vracia (claims, nil) pri úspešnej validácii alebo (nil, err) inak.
// Caller (sdk.GetUser) interpretuje err ako "neprihlásený" — žiadne 401
// sa neposiela automaticky, to je úloha RequireAuth middleware-u.
func ValidateJWT(boxID, token string) (*SessionClaims, error) {
	if token == "" {
		return nil, errors.New("empty token")
	}
	pub, err := LoadAuthPubKey(boxID)
	if err != nil {
		return nil, fmt.Errorf("load pubkey: %w", err)
	}
	claims := &SessionClaims{}
	t, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return pub, nil
	})
	if err != nil {
		// Pubkey mohla rotovať — skús refresh raz a re-validate.
		// Detekujeme cez signature error string (jwt/v5 nemá distinct error type).
		if strings.Contains(err.Error(), "signature is invalid") ||
			strings.Contains(err.Error(), "verification error") {
			if rerr := RefreshAuthPubKey(boxID); rerr == nil {
				pub, _ = LoadAuthPubKey(boxID)
				if pub != nil {
					claims2 := &SessionClaims{}
					t2, err2 := jwt.ParseWithClaims(token, claims2, func(t *jwt.Token) (any, error) {
						return pub, nil
					})
					if err2 == nil && t2.Valid {
						if claims2.Issuer != "apiself-box-auth" {
							return nil, fmt.Errorf("unexpected issuer: %s", claims2.Issuer)
						}
						return claims2, nil
					}
				}
			}
		}
		return nil, err
	}
	if !t.Valid {
		return nil, errors.New("token invalid")
	}
	if claims.Issuer != "apiself-box-auth" {
		return nil, fmt.Errorf("unexpected issuer: %s", claims.Issuer)
	}
	return claims, nil
}

// extractToken hľadá JWT v requeste v poradí:
//
//   1. Cookie "apiself_auth" — manager-proxy mode + same-origin direct-port
//   2. Authorization: Bearer <token> — direct-port cross-origin
//
// Vracia "" ak ani jedno nie je prítomné.
func extractToken(r *http.Request) string {
	if c, err := r.Cookie("apiself_auth"); err == nil && c.Value != "" {
		return c.Value
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

// userFromClaims mappne JWT claims na sdk.User. Symetria s userFromHeaders
// aby caller-i (RequireRole, HasRole) nevideli rozdiel medzi cestami.
func userFromClaims(c *SessionClaims) User {
	role := c.Role
	if role == "" {
		role = "member"
	}
	return User{
		ID:            c.UserID,
		Email:         c.Email,
		Role:          role,
		IsAdmin:       c.IsOwner || role == "admin",
		IsOwner:       c.IsOwner,
		Authenticated: c.UserID != "",
	}
}
