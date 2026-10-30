// Package terminal stellt Open-Terminal-Instanzen bereit, eine pro (User, Policy, Context).
package terminal

import (
	"errors"
	"net"
	"strconv"
	"time"
)

// Port, auf dem Open Terminal im Container lauscht.
const Port = 8000

// Key bestimmt eine Instanz eindeutig, wie in terminals.
type Key struct {
	UserID    string
	PolicyID  string
	ContextID string
}

// Instance ist eine bereite Open-Terminal-Instanz.
type Instance struct {
	Name   string
	Host   string
	Port   int
	APIKey string
}

// Addr liefert host:port der Instanz.
func (i Instance) Addr() string {
	return net.JoinHostPort(i.Host, strconv.Itoa(i.Port))
}

// URL liefert die Basis-URL der Instanz, scheme ist "http" oder "ws".
func (i Instance) URL(scheme string) string {
	return scheme + "://" + i.Addr()
}

// Spec sind die gespeicherten Daten einer Policy (PolicyData in openapi.json).
// nil bedeutet: nur die Defaults aus Config.
type Spec map[string]any

// Config enthält die Defaults für Instanzen, deren Policy nichts anderes vorgibt.
type Config struct {
	Backend            string // "kubernetes" oder "docker", setzt der Konstruktor
	Image              string
	IdleTimeoutMinutes int
	ProvisionTimeout   time.Duration

	// Nur Kubernetes.
	Namespace    string
	StorageClass string

	// Nur Docker: Netz, in dem chatbox und die Terminal-Container hängen.
	DockerNetwork string
}

// Info beschreibt eine Instanz für die Admin-API.
type Info struct {
	Key
	UID                string
	Name               string
	Host               string
	Status             string // running oder stopped
	LastActive         time.Time
	IdleSeconds        *int
	IdleTimeoutMinutes int
}

// Filter wählt Instanzen aus. nil-Felder passen auf alles.
type Filter struct {
	UserID, PolicyID, ContextID *string
}

func (f Filter) matches(k Key) bool {
	return (f.UserID == nil || *f.UserID == k.UserID) &&
		(f.PolicyID == nil || *f.PolicyID == k.PolicyID) &&
		(f.ContextID == nil || *f.ContextID == k.ContextID)
}

// RefreshResult zählt, was Refresh getan hat (RefreshResponse in openapi.json).
type RefreshResult struct {
	Matched, Refreshed, Reset, SkippedActive int
}

// ErrUnavailable liefert das Backend, wenn die Plattform nicht erreichbar ist.
var ErrUnavailable = errors.New("Terminal-Backend nicht verfügbar")
