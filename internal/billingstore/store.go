package billingstore

import (
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound           = errors.New("billing resource not found")
	ErrConflict           = errors.New("billing resource conflict")
	ErrIncomplete         = errors.New("billing evidence incomplete")
	ErrInvoiceImmutable   = errors.New("issued invoice is immutable")
	ErrPricingUnavailable = errors.New("pricing version unavailable")
)

type Store struct {
	db *pgxpool.Pool
}

func New(db *pgxpool.Pool) *Store { return &Store{db: db} }

type rowScanner interface {
	Scan(...any) error
}

func mapNotFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func required(value string) bool { return strings.TrimSpace(value) != "" }

type Page struct {
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
	Total  int `json:"total"`
}
