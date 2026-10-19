// Package product declares lockmq's names for lockkit.
package product

import "github.com/DavidGodefroid/lockkit/kit"

// P is lockmq as a lockkit product: its binary, directories, console
// account, clients group, keychain service and environment prefix.
var P = kit.Product{
	Name:            "lockmq",
	Tagline:         "Your AI reads the queue. You hold the key.",
	Account:         "lockmq",
	ClientsGroup:    "lockmq-clients",
	KeychainService: "lockmq",
	EnvPrefix:       "LOCKMQ_",
}
