package types

import "encoding/json"

// Contact represents a Dialpad shared or local contact.
// Maps to the ContactProto schema from the API.
type Contact struct {
	ID           string   `json:"id"`
	DisplayName  string   `json:"display_name"`
	FirstName    string   `json:"first_name,omitempty"`
	LastName     string   `json:"last_name,omitempty"`
	Emails       []string `json:"emails,omitempty"`
	PrimaryEmail string   `json:"primary_email,omitempty"`
	Phones       []string `json:"phones,omitempty"`
	PrimaryPhone string   `json:"primary_phone,omitempty"`
	CompanyName  string   `json:"company_name,omitempty"`
	JobTitle     string   `json:"job_title,omitempty"`
	Extension    string   `json:"extension,omitempty"`
	OwnerID      string   `json:"owner_id,omitempty"`
	Type         string   `json:"type,omitempty"` // "local" or "shared"
	URLs         []string `json:"urls,omitempty"`
}

// User represents a Dialpad user.
type User struct {
	ID          json.Number `json:"id"`
	DisplayName string      `json:"display_name"`
	FirstName   string      `json:"first_name,omitempty"`
	LastName    string      `json:"last_name,omitempty"`
	Email       string      `json:"email,omitempty"`
	Emails      []string    `json:"emails,omitempty"`
	Phones      []string    `json:"phones,omitempty"`
	AvatarURL   string      `json:"avatar_url,omitempty"`
	State       string      `json:"state,omitempty"` // active, cancelled, deleted, pending, suspended
	IsAdmin     bool        `json:"is_admin,omitempty"`
	OfficeID    json.Number `json:"office_id,omitempty"`
	LicenseType string     `json:"license_type,omitempty"`
}
