/*
Copyright 2026 TechDufus.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package celx compiles and evaluates the CEL expressions of an OmpTrigger:
// alert and event filters and the generic-adapter mapping.
//
// Every expression has a Role. The role fixes the single input variable
// (alert, event or payload) and the type the expression must produce, so a
// trigger's Ready check and the webhook handler share one definition. Compiled
// programs are safe for concurrent use and bounded by CostLimit.
package celx

import (
	"fmt"
	"reflect"
	"sync"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/ext"
	"google.golang.org/protobuf/types/known/structpb"
)

// CostLimit is the evaluation cost budget of every program.
const CostLimit = 100000

// Role identifies what an expression is used for.
type Role string

// Expression roles. The comment names the input variable and the output type.
const (
	RoleAlertFilter        Role = "alertFilter"         // alert -> bool
	RoleEventFilter        Role = "filter"              // event -> bool
	RoleMappingEvents      Role = "mapping.events"      // payload -> list
	RoleMappingKey         Role = "mapping.key"         // payload -> string
	RoleMappingTitle       Role = "mapping.title"       // payload -> string
	RoleMappingSeverity    Role = "mapping.severity"    // payload -> string
	RoleMappingStatus      Role = "mapping.status"      // payload -> string
	RoleMappingFingerprint Role = "mapping.fingerprint" // payload -> string
)

type output int

const (
	outBool output = iota
	outString
	outList
)

func (o output) String() string {
	switch o {
	case outBool:
		return "bool"
	case outString:
		return "string"
	default:
		return "list"
	}
}

type roleSpec struct {
	variable string
	out      output
}

const (
	varAlert   = "alert"
	varEvent   = "event"
	varPayload = "payload"
)

var roles = map[Role]roleSpec{
	RoleAlertFilter:        {varAlert, outBool},
	RoleEventFilter:        {varEvent, outBool},
	RoleMappingEvents:      {varPayload, outList},
	RoleMappingKey:         {varPayload, outString},
	RoleMappingTitle:       {varPayload, outString},
	RoleMappingSeverity:    {varPayload, outString},
	RoleMappingStatus:      {varPayload, outString},
	RoleMappingFingerprint: {varPayload, outString},
}

// Compiler compiles one expression for a role. [Compile] is a Compiler, and
// [Cache.Compiler] returns one that caches.
type Compiler func(role Role, expr string) (*Program, error)

var envs = sync.OnceValues(func() (map[string]*cel.Env, error) {
	m := make(map[string]*cel.Env, 3)
	for _, v := range []string{varAlert, varEvent, varPayload} {
		t := cel.MapType(cel.StringType, cel.DynType)
		if v == varPayload {
			t = cel.DynType
		}
		env, err := cel.NewEnv(cel.OptionalTypes(), ext.Strings(), cel.Variable(v, t))
		if err != nil {
			return nil, fmt.Errorf("celx: build %s environment: %w", v, err)
		}
		m[v] = env
	}
	return m, nil
})

// Program is a compiled, type-checked expression. It is immutable and safe for
// concurrent use.
type Program struct {
	role Role
	expr string
	prg  cel.Program
}

// Role returns the role the program was compiled for.
func (p *Program) Role() Role { return p.role }

// Expr returns the source expression.
func (p *Program) Expr() string { return p.expr }

// Compile parses and type-checks expr for role. The error names the role and
// describes the problem (syntax, undeclared reference or wrong result type).
// An expression whose type is dynamic (for example payload.id) passes the type
// check and is verified at evaluation time.
func Compile(role Role, expr string) (*Program, error) {
	spec, ok := roles[role]
	if !ok {
		return nil, fmt.Errorf("celx: unknown expression role %q", role)
	}
	if expr == "" {
		return nil, fmt.Errorf("%s: expression is empty", role)
	}
	all, err := envs()
	if err != nil {
		return nil, err
	}
	env := all[spec.variable]
	ast, iss := env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return nil, fmt.Errorf("%s: %w", role, iss.Err())
	}
	if err := checkOutput(ast.OutputType(), spec.out); err != nil {
		return nil, fmt.Errorf("%s: %w", role, err)
	}
	prg, err := env.Program(ast, cel.CostLimit(CostLimit))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", role, err)
	}
	return &Program{role: role, expr: expr, prg: prg}, nil
}

func checkOutput(t *types.Type, want output) error {
	k := t.Kind()
	if k == types.DynKind {
		return nil
	}
	ok := false
	switch want {
	case outBool:
		ok = k == types.BoolKind
	case outString:
		ok = k == types.StringKind
	case outList:
		ok = k == types.ListKind
	}
	if !ok {
		return fmt.Errorf("expression must evaluate to %s, got %s", want, t)
	}
	return nil
}

func (p *Program) eval(input any, want output) (any, error) {
	if roles[p.role].out != want {
		return nil, fmt.Errorf("%s: program produces %s, not %s", p.role, roles[p.role].out, want)
	}
	out, _, err := p.prg.Eval(map[string]any{roles[p.role].variable: input})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p.role, err)
	}
	switch want {
	case outBool:
		b, ok := out.Value().(bool)
		if !ok {
			return nil, fmt.Errorf("%s: result is %s, want bool", p.role, out.Type())
		}
		return b, nil
	case outString:
		s, ok := out.Value().(string)
		if !ok {
			return nil, fmt.Errorf("%s: result is %s, want string", p.role, out.Type())
		}
		return s, nil
	default:
		v, err := out.ConvertToNative(reflect.TypeFor[*structpb.Value]())
		if err != nil {
			return nil, fmt.Errorf("%s: result is not JSON-representable: %w", p.role, err)
		}
		list, ok := v.(*structpb.Value).AsInterface().([]any)
		if !ok {
			return nil, fmt.Errorf("%s: result is %s, want list", p.role, out.Type())
		}
		return list, nil
	}
}

// Bool evaluates a bool-producing program (alertFilter or filter) against
// input, the JSON-like value of the alert or event. On any runtime error,
// including exceeding [CostLimit], it returns false and the error, so a caller
// can count the error and still treat the result as false.
func (p *Program) Bool(input any) (bool, error) {
	v, err := p.eval(input, outBool)
	if err != nil {
		return false, err
	}
	return v.(bool), nil
}

// String evaluates a string-producing mapping program against payload.
func (p *Program) String(payload any) (string, error) {
	v, err := p.eval(payload, outString)
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

// List evaluates the mapping.events program against payload. Elements are
// JSON-like values (map[string]any, []any, string, float64, bool, nil).
func (p *Program) List(payload any) ([]any, error) {
	v, err := p.eval(payload, outList)
	if err != nil {
		return nil, err
	}
	return v.([]any), nil
}

type cached struct {
	expr string
	prog *Program
}

type object struct {
	generation int64
	progs      map[Role]cached
}

type objectKey struct{ namespace, name string }

// Cache keeps compiled programs per object (namespace, name) and role, valid
// for one generation of the object. Moving to a newer generation drops every
// program of the older one. It is safe for concurrent use.
type Cache struct {
	mu      sync.Mutex
	objects map[objectKey]*object
}

// NewCache returns an empty cache.
func NewCache() *Cache {
	return &Cache{objects: map[objectKey]*object{}}
}

// Compiler returns a [Compiler] that serves programs for the given object
// generation from the cache and compiles on a miss. Compile errors are not
// cached. A request for a generation older than the cached one compiles
// without caching.
func (c *Cache) Compiler(namespace, name string, generation int64) Compiler {
	key := objectKey{namespace, name}
	return func(role Role, expr string) (*Program, error) {
		c.mu.Lock()
		if o := c.objects[key]; o != nil && o.generation == generation {
			if hit, ok := o.progs[role]; ok && hit.expr == expr {
				c.mu.Unlock()
				return hit.prog, nil
			}
		}
		c.mu.Unlock()

		prog, err := Compile(role, expr)
		if err != nil {
			return nil, err
		}

		c.mu.Lock()
		defer c.mu.Unlock()
		o := c.objects[key]
		switch {
		case o == nil || o.generation < generation:
			o = &object{generation: generation, progs: map[Role]cached{}}
			c.objects[key] = o
		case o.generation > generation:
			return prog, nil
		}
		o.progs[role] = cached{expr: expr, prog: prog}
		return prog, nil
	}
}

// Forget drops every program of the object, for example when it is deleted.
func (c *Cache) Forget(namespace, name string) {
	c.mu.Lock()
	delete(c.objects, objectKey{namespace, name})
	c.mu.Unlock()
}
