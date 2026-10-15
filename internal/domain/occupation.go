package domain

import "encoding/json"

type OccupationType string

const (
	OccupationStudent             OccupationType = "student"
	OccupationWorkingProfessional OccupationType = "working_professional"
	OccupationOther               OccupationType = "other"
)

// OccupationDetails holds type-specific onboarding fields (profile_schema_version 1).
type OccupationDetails struct {
	CollegeName           string `json:"college_name,omitempty"`
	Course                string `json:"course,omitempty"`
	Year                  string `json:"year,omitempty"`
	StudentIDText         string `json:"student_id_text,omitempty"`
	ParentGuardianName    string `json:"parent_guardian_name,omitempty"`
	ParentGuardianPhone   string `json:"parent_guardian_phone,omitempty"`
	EmployerName          string `json:"employer_name,omitempty"`
	WorkArea              string `json:"work_area,omitempty"`
	EmployeeIDText        string `json:"employee_id_text,omitempty"`
	ReferenceContactName  string `json:"reference_contact_name,omitempty"`
	ReferenceContactPhone string `json:"reference_contact_phone,omitempty"`
	OccupationNote        string `json:"occupation_note,omitempty"`
}

func ParseOccupationDetails(raw json.RawMessage) (OccupationDetails, error) {
	if len(raw) == 0 {
		return OccupationDetails{}, nil
	}
	var d OccupationDetails
	if err := json.Unmarshal(raw, &d); err != nil {
		return OccupationDetails{}, err
	}
	return d, nil
}

func (d OccupationDetails) JSON() json.RawMessage {
	b, _ := json.Marshal(d)
	return b
}
