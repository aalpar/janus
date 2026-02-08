/*
Copyright 2026.

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

package controller

import (
	"fmt"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

// evaluateCELCondition evaluates a CEL expression against a Kubernetes resource
// represented as a map. The resource is available as "object" in the expression.
func evaluateCELCondition(expression string, obj map[string]interface{}) (bool, error) {
	env, err := cel.NewEnv(
		cel.Variable("object", cel.DynType),
	)
	if err != nil {
		return false, fmt.Errorf("creating CEL environment: %w", err)
	}

	ast, issues := env.Compile(expression)
	if issues != nil && issues.Err() != nil {
		return false, fmt.Errorf("compiling CEL expression %q: %w", expression, issues.Err())
	}

	prg, err := env.Program(ast)
	if err != nil {
		return false, fmt.Errorf("creating CEL program: %w", err)
	}

	out, _, err := prg.Eval(map[string]interface{}{
		"object": obj,
	})
	if err != nil {
		return false, fmt.Errorf("evaluating CEL expression %q: %w", expression, err)
	}

	if out.Type() != types.BoolType {
		return false, fmt.Errorf("CEL expression %q returned %s, expected bool", expression, out.Type())
	}

	return out.Value().(bool), nil
}

// compileCELCondition pre-compiles a CEL expression and returns a function
// that evaluates it against a resource. Use this when evaluating the same
// expression multiple times (e.g., polling for readiness).
func compileCELCondition(expression string) (func(map[string]interface{}) (bool, error), error) {
	env, err := cel.NewEnv(
		cel.Variable("object", cel.DynType),
	)
	if err != nil {
		return nil, fmt.Errorf("creating CEL environment: %w", err)
	}

	ast, issues := env.Compile(expression)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("compiling CEL expression %q: %w", expression, issues.Err())
	}

	prg, err := env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("creating CEL program: %w", err)
	}

	return func(obj map[string]interface{}) (bool, error) {
		out, _, err := prg.Eval(map[string]interface{}{
			"object": obj,
		})
		if err != nil {
			return false, fmt.Errorf("evaluating CEL expression: %w", err)
		}
		if out.Type() != types.BoolType {
			return false, fmt.Errorf("CEL expression returned %s, expected bool", out.Type())
		}
		return out.(ref.Val).Value().(bool), nil
	}, nil
}
