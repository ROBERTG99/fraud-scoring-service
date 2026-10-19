package main

import (
	"net/http"
	"regexp"
	"strings"

	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
)

type route struct {
	re     *regexp.Regexp
	method string
	query  map[string]bool // accepted query parameters (request fields not bound to the path or body)
}

// httpRoutes reads the google.api.http rules from the fraud.v1 descriptors, so the Allow header on a 405
// always matches the gateway (and the OpenAPI generated from the same rules).
func httpRoutes() []route {
	var out []route
	for _, fd := range []protoreflect.FileDescriptor{fraudv1.File_fraud_v1_scorer_proto, fraudv1.File_fraud_v1_cases_proto} {
		svcs := fd.Services()
		for i := range svcs.Len() {
			methods := svcs.Get(i).Methods()
			for j := range methods.Len() {
				rule, ok := proto.GetExtension(methods.Get(j).Options(), annotations.E_Http).(*annotations.HttpRule)
				if !ok || rule == nil {
					continue
				}
				method, path := httpRule(rule)
				if path == "" {
					continue
				}
				query := map[string]bool{}
				if rule.Body != "*" {
					fields := methods.Get(j).Input().Fields()
					for k := range fields.Len() {
						name := string(fields.Get(k).Name())
						if !strings.Contains(path, "{"+name+"}") && name != rule.Body {
							query[name] = true
						}
					}
				}
				out = append(out, route{re: templateRegexp(path), method: method, query: query})
			}
		}
	}
	return out
}

func httpRule(r *annotations.HttpRule) (string, string) {
	switch p := r.Pattern.(type) {
	case *annotations.HttpRule_Get:
		return http.MethodGet, p.Get
	case *annotations.HttpRule_Post:
		return http.MethodPost, p.Post
	case *annotations.HttpRule_Put:
		return http.MethodPut, p.Put
	case *annotations.HttpRule_Patch:
		return http.MethodPatch, p.Patch
	case *annotations.HttpRule_Delete:
		return http.MethodDelete, p.Delete
	}
	return "", ""
}

var reVar = regexp.MustCompile(`\{[^}]+\}`)

// templateRegexp turns "/v1/cases/{case_id}:label" into ^/v1/cases/[^/:]+:label$.
func templateRegexp(tmpl string) *regexp.Regexp {
	parts := reVar.Split(tmpl, -1)
	for i := range parts {
		parts[i] = regexp.QuoteMeta(parts[i])
	}
	return regexp.MustCompile("^" + strings.Join(parts, "[^/:]+") + "$")
}

// match returns the route for method and path, and the methods routed for path (comma separated).
func match(routes []route, method, path string) (*route, string) {
	var found *route
	var allow []string
	for i := range routes {
		if routes[i].re.MatchString(path) {
			allow = append(allow, routes[i].method)
			if routes[i].method == method {
				found = &routes[i]
			}
		}
	}
	return found, strings.Join(allow, ", ")
}

// allowedMethods returns the methods routed for path, comma separated.
func allowedMethods(routes []route, path string) string {
	var m []string
	for _, r := range routes {
		if r.re.MatchString(path) {
			m = append(m, r.method)
		}
	}
	return strings.Join(m, ", ")
}
