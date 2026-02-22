package sdk

import "fmt"

// BoxConfig definuje, čo o sebe Box povie Core-u
type BoxConfig struct {
	ID      string
	Version string
	Name    string
}

// InitBox inicializuje základné nastavenia
func InitBox(conf BoxConfig) {
	fmt.Printf("📦 APISelf SDK: Inicializujem Box [%s] verzia %s\n", conf.Name, conf.Version)
	// Tu neskôr pribudne logika pre RSA check
}
