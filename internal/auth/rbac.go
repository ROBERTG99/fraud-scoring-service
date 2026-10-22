// Package auth verifies dev-issuer JWTs (EdDSA, JWKS) and enforces role-based access.
package auth

import (
	"fmt"
	"slices"
)

// Roles.
const (
	RoleAnalyst      = "analyst"
	RoleRiskEngineer = "risk_engineer"
	RoleMLEngineer   = "ml_engineer"
	RoleService      = "service"
)

// ConsoleRoles are the roles a dev-issuer token can carry.
var ConsoleRoles = []string{RoleAnalyst, RoleRiskEngineer, RoleMLEngineer}

const svc = "/fraud.v1.CaseService/"

// MethodRoles is the RBAC table: LabelCase analyst, TestRule risk_engineer, GetShadowSummary risk_engineer or
// ml_engineer, reads any console role. A method missing from the table is denied.
var MethodRoles = map[string][]string{
	svc + "ListCases":         ConsoleRoles,
	svc + "GetCase":           ConsoleRoles,
	svc + "GetTimeline":       ConsoleRoles,
	svc + "GetActiveRuleSet":  ConsoleRoles,
	svc + "StreamLiveMetrics": ConsoleRoles,
	svc + "LabelCase":         {RoleAnalyst},
	svc + "TestRule":          {RoleRiskEngineer},
	svc + "GetShadowSummary":  {RoleRiskEngineer, RoleMLEngineer},
}

// Authorize returns nil when role may call method.
func Authorize(method, role string) error {
	allowed, ok := MethodRoles[method]
	if !ok {
		return fmt.Errorf("method %s is not exposed to console roles", method)
	}
	if !slices.Contains(allowed, role) {
		return fmt.Errorf("role %s may not call %s", role, method)
	}
	return nil
}
