// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/go-interpreter/wagon/wasm"
	"golang.org/x/tools/go/packages"
)

func main() {
	if err := run(); err != nil {
		panic(err)
	}
}

func identifierFromString(str string) string {
	var ident string
	for _, r := range []rune(str) {
		if r > 0xff {
			panic("identifiers cannot include non-Latin1 characters")
		}
		if '0' <= r && r <= '9' {
			ident += string(r)
			continue
		}
		if 'a' <= r && r <= 'z' {
			ident += string(r)
			continue
		}
		if 'A' <= r && r <= 'Z' {
			ident += string(r)
			continue
		}
		ident += fmt.Sprintf("_%02x", r)
	}
	return ident
}

func namespaceFromPkg(pkg *packages.Package) string {
	ts := strings.Split(pkg.PkgPath, "/")
	for i, t := range ts {
		ts[i] = identifierFromString(t)
	}
	return strings.Join(ts, ".")
}

type Func struct {
	Funcs []*Func
	Types []*Type
	Type  *Type
	Body  *wasm.FunctionBody
	Index int
	Name  string
}

func (f *Func) Identifier() string {
	return identifierFromString(f.Name)
}

var funcTmpl = template.Must(template.New("func").Parse(`// OriginalName: {{.OriginalName}}
// Index:        {{.Index}}
internal {{.ReturnType}} {{.Name}}({{.Args}})
{
{{range .Locals}}    {{.}}
{{end}}
{{range .Body}}    {{.}}
{{end}}}`))

func wasmTypeToReturnType(v wasm.ValueType) ReturnType {
	switch v {
	case wasm.ValueTypeI32:
		return ReturnTypeI32
	case wasm.ValueTypeI64:
		return ReturnTypeI64
	case wasm.ValueTypeF32:
		return ReturnTypeF32
	case wasm.ValueTypeF64:
		return ReturnTypeF64
	default:
		panic("not reached")
	}
}

func (f *Func) CSharp(indent string) (string, error) {
	var retType ReturnType
	switch ts := f.Type.Sig.ReturnTypes; len(ts) {
	case 0:
		retType = ReturnTypeVoid
	case 1:
		retType = wasmTypeToReturnType(ts[0])
	default:
		return "", fmt.Errorf("the number of return values must be 0 or 1 but %d", len(ts))
	}

	var args []string
	for i, t := range f.Type.Sig.ParamTypes {
		args = append(args, fmt.Sprintf("%s local%d", wasmTypeToReturnType(t).CSharp(), i))
	}

	var locals []string
	var body []string
	if f.Body != nil {
		var idx int
		for _, e := range f.Body.Locals {
			for i := 0; i < int(e.Count); i++ {
				locals = append(locals, fmt.Sprintf("%s local%d = 0;", wasmTypeToReturnType(e.Type).CSharp(), idx+len(f.Type.Sig.ParamTypes)))
				idx++
			}
		}
		var err error
		body, err = opsToCSharp(f.Body.Code, f.Type.Sig, f.Funcs, f.Types)
		if err != nil {
			return "", err
		}
	}

	var buf bytes.Buffer
	if err := funcTmpl.Execute(&buf, struct {
		OriginalName string
		Name         string
		Index        int
		ReturnType   string
		Args         string
		Locals       []string
		Body         []string
	}{
		OriginalName: f.Name,
		Name:         identifierFromString(f.Name),
		Index:        f.Index,
		ReturnType:   retType.CSharp(),
		Args:         strings.Join(args, ", "),
		Locals:       locals,
		Body:         body,
	}); err != nil {
		return "", err
	}

	// Add indentations
	var lines []string
	for _, line := range strings.Split(buf.String(), "\n") {
		lines = append(lines, indent+line)
	}
	return strings.Join(lines, "\n") + "\n", nil
}

type Global struct {
	Type  wasm.ValueType
	Index int
	Init  int
}

func (g *Global) CSharp(indent string) string {
	return fmt.Sprintf("%sprivate %s global%d = %d;", indent, wasmTypeToReturnType(g.Type).CSharp(), g.Index, g.Init)
}

type Type struct {
	Sig   *wasm.FunctionSig
	Index int
}

func (t *Type) CSharp(indent string) (string, error) {
	var retType ReturnType
	switch ts := t.Sig.ReturnTypes; len(ts) {
	case 0:
		retType = ReturnTypeVoid
	case 1:
		retType = wasmTypeToReturnType(ts[0])
	default:
		return "", fmt.Errorf("the number of return values must be 0 or 1 but %d", len(ts))
	}

	var args []string
	for i, t := range t.Sig.ParamTypes {
		args = append(args, fmt.Sprintf("%s arg%d", wasmTypeToReturnType(t).CSharp(), i))
	}

	return fmt.Sprintf("%sprivate delegate %s Type%d(%s);", indent, retType.CSharp(), t.Index, strings.Join(args, ", ")), nil
}

func run() error {
	tmp, err := ioutil.TempDir("", "go2dotnet-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	wasmpath := filepath.Join(tmp, "tmp.wasm")

	// TODO: Detect the last argument is path or not
	pkgname := os.Args[len(os.Args)-1]

	args := append([]string{"build"}, os.Args[1:]...)
	args = append(args[:len(args)-1], "-o="+wasmpath, pkgname)
	cmd := exec.Command("go", args...)
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm")
	if err := cmd.Run(); err != nil {
		return err
	}

	f, err := os.Open(wasmpath)
	if err != nil {
		return err
	}
	defer f.Close()

	mod, err := wasm.ReadModule(f, nil)
	if err != nil {
		return err
	}

	var types []*Type
	for i, e := range mod.Types.Entries {
		e := e
		types = append(types, &Type{
			Sig:   &e,
			Index: i,
		})
	}

	var ifs []*Func
	var fs []*Func
	for i, f := range mod.FunctionIndexSpace {
		// There is a bug that signature and body are shifted (go-interpreter/wagon#190).
		// TODO: Avoid using FunctionIndexSpace?
		if f.Name == "" {
			ifs = append(ifs, &Func{
				Type:  types[mod.Import.Entries[i].Type.(wasm.FuncImport).Type],
				Index: i,
				Name:  mod.Import.Entries[i].FieldName,
			})
			continue
		}

		f2 := mod.FunctionIndexSpace[i-len(mod.Import.Entries)]
		fs = append(fs, &Func{
			Type:  types[mod.Function.Types[i-len(mod.Import.Entries)]],
			Body:  f2.Body,
			Index: i,
			Name:  f.Name,
		})
	}
	allfs := append(ifs, fs...)
	for _, f := range ifs {
		f.Funcs = allfs
		f.Types = types
	}
	for _, f := range fs {
		f.Funcs = allfs
		f.Types = types
	}

	var globals []*Global
	for i, e := range mod.Global.Globals {
		// TODO: Consider mutability.
		// TODO: Use e.Type.Init.
		globals = append(globals, &Global{
			Type:  e.Type.Type,
			Index: i,
			Init:  0,
		})
	}

	pkgs, err := packages.Load(nil, pkgname)
	if err != nil {
		return err
	}

	namespace := namespaceFromPkg(pkgs[0])
	class := identifierFromString(pkgs[0].Name)

	if err := csTmpl.Execute(os.Stdout, struct {
		Namespace   string
		Class       string
		ImportFuncs []*Func
		Funcs       []*Func
		Globals     []*Global
		Types       []*Type
		Table       [][]uint32
	}{
		Namespace:   namespace,
		Class:       class,
		ImportFuncs: ifs,
		Funcs:       fs,
		Globals:     globals,
		Types:       types,
		Table:       mod.TableIndexSpace,
	}); err != nil {
		return err
	}

	return nil
}

var csTmpl = template.Must(template.New("out.cs").Parse(`// Code generated by go2dotnet. DO NOT EDIT.

// TODO: Remove this.
#pragma warning disable 162
#pragma warning disable 168
#pragma warning disable 219
#pragma warning disable 414

using System;
using System.Diagnostics;

namespace {{.Namespace}}
{
    sealed class Import
    {
{{- range $value := .ImportFuncs}}
{{$value.CSharp "        "}}{{end}}    }

    sealed class Go_{{.Class}}
    {
        public Go_{{.Class}}()
        {
             initializeFuncs_();
        }

{{range $value := .Globals}}{{$value.CSharp "        "}}
{{end}}
{{range $value := .Funcs}}{{$value.CSharp "        "}}
{{end}}
{{range $value := .Types}}{{$value.CSharp "        "}}
{{end}}        private static readonly uint[][] table_ = {
{{range $value := .Table}}            new uint[] { {{- range $value2 := $value}}{{$value2}}, {{end}}},
{{end}}        };

        private object[] funcs_;

        private void initializeFuncs_()
        {
            funcs_ = new object[] {
{{range $value := .ImportFuncs}}                null,
{{end}}{{range $value := .Funcs}}                (Type{{.Type.Index}})({{.Identifier}}),
{{end}}            };
        }
    }
}
`))
