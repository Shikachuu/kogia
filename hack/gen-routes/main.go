// gen-routes reads api/swagger.yaml and generates:
//   - Handler interface with one method per operation
//   - RegisterRoutes() wiring all routes to a stdlib net/http.ServeMux
//   - NotImplemented struct providing default 501 for every method
package main

import (
	"bytes"
	"go/format"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"github.com/go-openapi/loads"
	"github.com/go-openapi/spec"
)

const (
	specFile   = "api/swagger.yaml"
	outputFile = "internal/api/gen/routes.go"
)

type route struct {
	Method      string // "GET", "POST", etc.
	Path        string // swagger path, e.g. "/containers/{name}/json"
	OperationID string // e.g. "ContainerInspect"
	Summary     string // one-line description
}

// MuxPattern returns the Go 1.22 ServeMux route pattern, e.g. `"GET "+"/containers/{id}/json"`.
func (r route) MuxPattern() string {
	return r.Method + " "
}

// Comment returns the interface method comment line.
func (r route) Comment() string {
	c := r.Method + " " + r.Path
	if r.Summary != "" {
		c += " — " + r.Summary
	}

	return c
}

func main() {
	root := moduleRoot()

	doc, err := loads.Spec(filepath.Join(root, specFile))
	if err != nil {
		log.Fatalf("loading spec: %v", err)
	}

	swagger := doc.Spec()
	routes := collectRoutes(swagger)

	sort.Slice(routes, func(i, j int) bool {
		return routes[i].OperationID < routes[j].OperationID
	})

	src := generate(root, routes)

	formatted, err := format.Source(src)
	if err != nil {
		log.Printf("WARN: gofmt failed, writing raw output: %v", err)

		formatted = src
	}

	outPath := filepath.Join(root, outputFile)

	if mkdirErr := os.MkdirAll(filepath.Dir(outPath), 0o750); mkdirErr != nil {
		log.Fatalf("mkdir: %v", mkdirErr)
	}

	if writeErr := os.WriteFile(outPath, formatted, 0o600); writeErr != nil {
		log.Fatalf("writing %s: %v", outPath, writeErr)
	}

	log.Printf("wrote %s with %d operations", outputFile, len(routes))
}

func collectRoutes(swagger *spec.Swagger) []route {
	var routes []route

	for path := range swagger.Paths.Paths {
		item := swagger.Paths.Paths[path]

		for method, op := range pathOperations(&item) {
			if op.ID == "" {
				log.Printf("WARN: %s %s has no operationId, skipping", method, path)

				continue
			}

			routes = append(routes, route{
				Method:      method,
				Path:        path,
				OperationID: op.ID,
				Summary:     op.Summary,
			})
		}
	}

	return routes
}

func pathOperations(item *spec.PathItem) map[string]*spec.Operation {
	ops := map[string]*spec.Operation{}

	if item.Get != nil {
		ops["GET"] = item.Get
	}

	if item.Post != nil {
		ops["POST"] = item.Post
	}

	if item.Put != nil {
		ops["PUT"] = item.Put
	}

	if item.Delete != nil {
		ops["DELETE"] = item.Delete
	}

	if item.Head != nil {
		ops["HEAD"] = item.Head
	}

	if item.Patch != nil {
		ops["PATCH"] = item.Patch
	}

	if item.Options != nil {
		ops["OPTIONS"] = item.Options
	}

	return ops
}

const tmplFile = "hack/gen-routes/routes.go.tmpl"

func generate(root string, routes []route) []byte {
	tmpl, err := template.ParseFiles(filepath.Join(root, tmplFile))
	if err != nil {
		log.Fatalf("parsing template %s: %v", tmplFile, err)
	}

	var buf bytes.Buffer

	if err := tmpl.Execute(&buf, routes); err != nil {
		log.Fatalf("executing template: %v", err)
	}

	return buf.Bytes()
}

// moduleRoot returns the Go module root directory.
func moduleRoot() string {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		log.Fatalf("go env GOMOD: %v", err)
	}

	gomod := strings.TrimSpace(string(out))
	if gomod == "" || gomod == os.DevNull {
		log.Fatal("not inside a Go module")
	}

	return filepath.Dir(gomod)
}
