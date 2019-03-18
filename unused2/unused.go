package unused

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"

	"honnef.co/go/tools/go/types/typeutil"
	"honnef.co/go/tools/lint"
	"honnef.co/go/tools/lint/lintdsl"
	"honnef.co/go/tools/ssa"
)

// OPT(dh): optimize graph by not storing irrelevant nodes. storing
// basic types, empty signatures etc doesn't add any information to
// the graph.
//
// OPT(dh): deduplicate edges
//
// OPT(dh): don't track function calls into external packages

// TODO(dh): conversions between structs mark fields as used, but the
// conversion itself isn't part of that subgraph. even if the function
// containing the conversion is unused, the fields will be marked as
// used.

const debug = false

/*

TODO known reflect
TODO error interface

- packages use:
  - (2525) exported named types
  - (5252) exported functions
  - exported variables
  - (321) exported constants
  - (6719) init functions
  - TODO functions exported to cgo
  - (4644) the main function iff in the main package

- named types use:
  - (9728) exported methods

- variables and constants use:
  - their types

- functions use:
  - (1663) all their arguments, return parameters and receivers
  - (9567) anonymous functions defined beneath them
  - (2521) closures and bound methods.
    this implements a simplified model where a function is used merely by being referenced, even if it is never called.
    that way we don't have to keep track of closures escaping functions.
  - (8103) functions they return. we assume that someone else will call the returned function
  - (9681) functions/interface methods they call
  - types they instantiate or convert to
  - (1323) fields they access
  - types of all instructions

- conversions use:
  - (6885) when converting between two equivalent structs, the fields in
    either struct use each other. the fields are relevant for the
    conversion, but only if the fields are also accessed outside the
    conversion.
  - (4029) when converting to or from unsafe.Pointer, mark all fields as used.

- structs use:
  - (4946) fields of type NoCopy sentinel
  - (2701) exported fields
  - embedded fields that help implement interfaces (either fully implements it, or contributes required methods) (recursively)
  - (6090) embedded fields that have exported methods (recursively)
  - (8728) embedded structs that have exported fields (recursively)

- field accesses use fields

- interfaces use:
  - (3393) all their methods. we really have no idea what is going on with interfaces.
  - matching methods on types that implement this interface.
    we assume that types are meant to implement as many interfaces as possible.
    takes into consideration embedding into possibly unnamed types.


- Inherent uses:
  - thunks and other generated wrappers call the real function
  - (254) instructions use their types
  - (5749) variables use their types
  - (6108) types use their underlying and element types
  - (853) conversions use the type they convert to

- things named _ are used
*/

func assert(b bool) {
	if !b {
		panic("failed assertion")
	}
}

func NewLintChecker(c *Checker) *LintChecker {
	l := &LintChecker{
		c: c,
	}
	return l
}

type LintChecker struct {
	c *Checker
}

func (*LintChecker) Name() string   { return "unused" }
func (*LintChecker) Prefix() string { return "U" }

func (l *LintChecker) Init(*lint.Program) {}
func (l *LintChecker) Checks() []lint.Check {
	return []lint.Check{
		{ID: "U1000", FilterGenerated: true, Fn: l.Lint},
	}
}

func typString(obj types.Object) string {
	switch obj := obj.(type) {
	case *types.Func:
		return "func"
	case *types.Var:
		if obj.IsField() {
			return "field"
		}
		return "var"
	case *types.Const:
		return "const"
	case *types.TypeName:
		return "type"
	default:
		return "identifier"
	}
}

func (l *LintChecker) Lint(j *lint.Job) {
	unused := l.c.Check(j.Program, j)
	for _, u := range unused {
		name := u.Obj.Name()
		if sig, ok := u.Obj.Type().(*types.Signature); ok && sig.Recv() != nil {
			switch sig.Recv().Type().(type) {
			case *types.Named, *types.Pointer:
				typ := types.TypeString(sig.Recv().Type(), func(*types.Package) string { return "" })
				if len(typ) > 0 && typ[0] == '*' {
					name = fmt.Sprintf("(%s).%s", typ, u.Obj.Name())
				} else if len(typ) > 0 {
					name = fmt.Sprintf("%s.%s", typ, u.Obj.Name())
				}
			}
		}
		j.Errorf(u.Obj, "%s %s is unused", typString(u.Obj), name)
	}
}

type Unused struct {
	Obj      types.Object
	Position token.Position
}

func NewChecker() *Checker {
	return &Checker{}
}

type Checker struct{}

func (c *Checker) Check(prog *lint.Program, j *lint.Job) []Unused {
	scopes := map[*types.Scope]*ssa.Function{}
	for _, fn := range j.Program.InitialFunctions {
		if fn.Object() != nil {
			scope := fn.Object().(*types.Func).Scope()
			scopes[scope] = fn
		}
	}

	var out []Unused
	for _, pkg := range prog.InitialPackages {
		graph := NewGraph(pkg.SSA)
		graph.job = j
		graph.scopes = scopes
		graph.entry(pkg.TypesInfo)

		graph.color(graph.Root)
		quieten := func(node *Node) {
			if node.seen {
				return
			}
			switch obj := node.obj.(type) {
			case *types.Var:
				if !obj.IsField() {
					// Only flag fields. Variables are either
					// arguments or local variables, neither of which
					// should ever be reported.
					node.quiet = true
				}
			case *types.Named:
				for i := 0; i < obj.NumMethods(); i++ {
					m := pkg.SSA.Prog.FuncValue(obj.Method(i))
					if node, ok := graph.nodeMaybe(m); ok {
						node.quiet = true
					}
				}
			case *types.Struct:
				for i := 0; i < obj.NumFields(); i++ {
					if node, ok := graph.nodeMaybe(obj.Field(i)); ok {
						node.quiet = true
					}
				}
			}
		}
		for _, node := range graph.Nodes {
			quieten(node)
		}
		graph.TypeNodes.Iterate(func(_ types.Type, value interface{}) {
			quieten(value.(*Node))
		})

		report := func(node *Node) {
			if node.seen {
				return
			}
			if node.quiet {
				if debug {
					fmt.Printf("n%d [color=purple];\n", node.id)
				}
				return
			}
			if debug {
				fmt.Printf("n%d [color=red];\n", node.id)
			}
			switch obj := node.obj.(type) {
			case types.Object:
				if obj.Pkg() == pkg.Package.Types {
					pos := prog.Fset().Position(obj.Pos())
					out = append(out, Unused{
						Obj:      obj,
						Position: pos,
					})
				}
			case *ssa.Function:
				if obj == nil {
					// TODO(dh): how does this happen?
					return
				}

				// OPT(dh): objects in other packages should never make it into the graph
				if obj.Object() != nil && obj.Object().Pkg() == pkg.Types {
					pos := prog.Fset().Position(obj.Pos())
					out = append(out, Unused{
						Obj:      obj.Object(),
						Position: pos,
					})
				}
			default:
				if debug {
					fmt.Printf("n%d [color=gray];\n", node.id)
				}
			}
		}
		for _, node := range graph.Nodes {
			report(node)
		}
		graph.TypeNodes.Iterate(func(_ types.Type, value interface{}) {
			report(value.(*Node))
		})
	}
	return out
}

type Graph struct {
	job     *lint.Job
	pkg     *ssa.Package
	msCache typeutil.MethodSetCache
	scopes  map[*types.Scope]*ssa.Function

	nodeCounter int

	Root      *Node
	TypeNodes typeutil.Map
	Nodes     map[interface{}]*Node

	seenTypes typeutil.Map
	seenFns   map[*ssa.Function]struct{}
}

func NewGraph(pkg *ssa.Package) *Graph {
	g := &Graph{
		pkg:     pkg,
		Nodes:   map[interface{}]*Node{},
		seenFns: map[*ssa.Function]struct{}{},
	}
	g.Root = g.newNode(nil)
	if debug {
		fmt.Printf("n%d [label=\"Root\"];\n", g.Root.id)
	}

	return g
}

func (g *Graph) color(root *Node) {
	if root.seen {
		return
	}
	root.seen = true
	for other := range root.used {
		g.color(other)
	}
}

type Node struct {
	obj  interface{}
	id   int
	used map[*Node]string

	seen  bool
	quiet bool
}

func (g *Graph) nodeMaybe(obj interface{}) (*Node, bool) {
	if t, ok := obj.(types.Type); ok {
		if v := g.TypeNodes.At(t); v != nil {
			return v.(*Node), true
		}
		return nil, false
	}
	if node, ok := g.Nodes[obj]; ok {
		return node, true
	}
	return nil, false
}

func (g *Graph) node(obj interface{}) (node *Node, new bool) {
	if t, ok := obj.(types.Type); ok {
		if v := g.TypeNodes.At(t); v != nil {
			return v.(*Node), false
		}
		node := g.newNode(obj)
		g.TypeNodes.Set(t, node)
		return node, true
	}
	if node, ok := g.Nodes[obj]; ok {
		return node, false
	}
	node = g.newNode(obj)
	g.Nodes[obj] = node
	return node, true
}

func (g *Graph) newNode(obj interface{}) *Node {
	g.nodeCounter++
	return &Node{
		obj:  obj,
		id:   g.nodeCounter,
		used: map[*Node]string{},
	}
}

func (n *Node) use(node *Node, reason string) (new bool) {
	assert(node != nil)
	if s, ok := n.used[node]; ok && s == reason {
		return false
	}
	n.used[node] = reason
	return true
}

// isIrrelevant reports whether an object's presence in the graph is
// of any relevance. A lot of objects will never have outgoing edges,
// nor meaningful incoming ones. Examples are basic types and empty
// signatures, among many others.
//
// Dropping these objects should have no effect on correctness, but
// may improve performance. It also helps with debugging, as it
// greatly reduces the size of the graph.
func isIrrelevant(obj interface{}) bool {
	if obj, ok := obj.(types.Object); ok {
		switch obj := obj.(type) {
		case *types.Var:
			return !obj.IsField() && isIrrelevant(obj.Type())
		default:
			return false
		}
	}
	if T, ok := obj.(types.Type); ok {
		T = lintdsl.Dereference(T)
		switch T := T.(type) {
		case *types.Array:
			return isIrrelevant(T.Elem())
		case *types.Slice:
			return isIrrelevant(T.Elem())
		case *types.Basic:
			return true
		case *types.Tuple:
			for i := 0; i < T.Len(); i++ {
				if !isIrrelevant(T.At(i).Type()) {
					return false
				}
			}
			return true
		case *types.Signature:
			if T.Recv() != nil {
				return false
			}
			for i := 0; i < T.Params().Len(); i++ {
				if !isIrrelevant(T.Params().At(i)) {
					return false
				}
			}
			for i := 0; i < T.Results().Len(); i++ {
				if !isIrrelevant(T.Results().At(i)) {
					return false
				}
			}
			return true
		case *types.Interface:
			return T.NumMethods() == 0
		default:
			return false
		}
	}
	return false
}

func (g *Graph) see(obj interface{}) {
	if isIrrelevant(obj) {
		return
	}

	assert(obj != nil)
	if obj, ok := obj.(types.Object); ok && obj.Pkg() != nil {
		if obj.Pkg() != g.pkg.Pkg {
			return
		}
	}

	// add new node to graph
	node, new := g.node(obj)
	if debug && new {
		fmt.Printf("n%d [label=%q];\n", node.id, obj)
	}
}

func (g *Graph) use(used, by interface{}, reason string) {
	if isIrrelevant(used) {
		return
	}

	assert(used != nil)
	if _, ok := used.(*types.Func); ok {
		assert(g.pkg.Prog.FuncValue(used.(*types.Func)) == nil)
	}
	if _, ok := by.(*types.Func); ok {
		assert(g.pkg.Prog.FuncValue(by.(*types.Func)) == nil)
	}
	if obj, ok := used.(types.Object); ok && obj.Pkg() != nil {
		if obj.Pkg() != g.pkg.Pkg {
			return
		}
	}
	if obj, ok := by.(types.Object); ok && obj.Pkg() != nil {
		if obj.Pkg() != g.pkg.Pkg {
			return
		}
	}
	usedNode, new := g.node(used)
	assert(!new)
	if by == nil {
		new := g.Root.use(usedNode, reason)
		if debug && new {
			fmt.Printf("n%d -> n%d [label=%q];\n", g.Root.id, usedNode.id, reason)
		}
	} else {
		byNode, new := g.node(by)
		assert(!new)
		new = byNode.use(usedNode, reason)
		if debug && new {
			fmt.Printf("n%d -> n%d [label=%q];\n", byNode.id, usedNode.id, reason)
		}

	}
}

func (g *Graph) seeAndUse(used, by interface{}, reason string) {
	g.see(used)
	g.use(used, by, reason)
}

func (g *Graph) entry(tinfo *types.Info) {
	// TODO rename Entry

	surroundingFunc := func(obj types.Object) *ssa.Function {
		scope := obj.Parent()
		for scope != nil {
			if fn := g.scopes[scope]; fn != nil {
				return fn
			}
			scope = scope.Parent()
		}
		return nil
	}

	// SSA form won't tell us about locally scoped types that aren't
	// being used. Walk the list of Defs to get all named types.
	//
	// SSA form also won't tell us about constants; use Defs and Uses
	// to determine which constants exist and which are being used.
	for _, obj := range tinfo.Defs {
		switch obj := obj.(type) {
		case *types.TypeName:
			g.see(obj)
			g.typ(obj.Type())
		case *types.Const:
			g.see(obj)
			fn := surroundingFunc(obj)
			if fn == nil && obj.Exported() {
				// (321) packages use exported constants
				g.use(obj, nil, "exported constant")
			}
			g.typ(obj.Type())
			g.seeAndUse(obj.Type(), obj, "constant type")
		}
	}

	// Find constants being used inside functions
	handledConsts := map[*ast.Ident]struct{}{}
	for _, fn := range g.job.Program.InitialFunctions {
		if fn.Pkg != g.pkg {
			continue
		}
		g.see(fn)
		node := fn.Syntax()
		if node == nil {
			continue
		}
		ast.Inspect(node, func(node ast.Node) bool {
			ident, ok := node.(*ast.Ident)
			if !ok {
				return true
			}

			obj, ok := tinfo.Uses[ident]
			if !ok {
				return true
			}
			switch obj := obj.(type) {
			case *types.Const:
				g.seeAndUse(obj, fn, "used constant")
			}
			return true
		})
	}
	// Find constants being used in non-function contexts
	for ident, obj := range tinfo.Uses {
		_, ok := obj.(*types.Const)
		if !ok {
			continue
		}
		if _, ok := handledConsts[ident]; ok {
			continue
		}
		g.seeAndUse(obj, nil, "used constant")
	}

	for _, m := range g.pkg.Members {
		switch m := m.(type) {
		case *ssa.NamedConst:
			// XXX
		case *ssa.Global:
			// XXX
		case *ssa.Function:
			g.see(m)
			if m.Name() == "init" {
				// (6719) packages use init functions
				g.use(m, nil, "init function")
			}
			// This branch catches top-level functions, not methods.
			if m.Object() != nil && m.Object().Exported() {
				// (5252) packages use exported functions
				g.use(m, nil, "exported top-level function")
			}
			if m.Name() == "main" && g.pkg.Pkg.Name() == "main" {
				// (4644) packages use the main function iff in the main package
				g.use(m, nil, "main function")
			}
			g.function(m)
		case *ssa.Type:
			if m.Object() != nil {
				g.see(m.Object())
				if m.Object().Exported() {
					// (2525) packages use exported named types
					g.use(m.Object(), nil, "exported top-level type")
				}
			}
			g.typ(m.Type())
		default:
			panic(fmt.Sprintf("unreachable: %T", m))
		}
	}

	var ifaces []*types.Interface
	var notIfaces []types.Type

	g.seenTypes.Iterate(func(t types.Type, _ interface{}) {
		switch t := t.(type) {
		case *types.Interface:
			ifaces = append(ifaces, t)
		default:
			if _, ok := t.Underlying().(*types.Interface); !ok {
				notIfaces = append(notIfaces, t)
			}
		}
	})

	for _, iface := range ifaces {
		for _, t := range notIfaces {
			if g.implements(t, iface) {
				for i := 0; i < iface.NumMethods(); i++ {
					// get the chain of embedded types that lead to the function implementing the interface
					ms := g.msCache.MethodSet(t)
					sel := ms.Lookup(g.pkg.Pkg, iface.Method(i).Name())
					obj := sel.Obj()
					path := sel.Index()
					assert(obj != nil)
					if len(path) > 1 {
						base := lintdsl.Dereference(t).Underlying().(*types.Struct)
						for _, idx := range path[:len(path)-1] {
							next := base.Field(idx)
							g.seeAndUse(next, base, "helps implement")
							base, _ = lintdsl.Dereference(next.Type()).Underlying().(*types.Struct)
						}
					}
					if fn := g.pkg.Prog.FuncValue(obj.(*types.Func)); fn != nil {
						// actual function
						g.seeAndUse(fn, iface, "implements")
					} else {
						// interface method
						g.seeAndUse(obj, iface, "implements")
					}
				}
			}
		}
	}
}

func (g *Graph) function(fn *ssa.Function) {
	// (1663) functions use all their arguments, return parameters and receivers
	g.seeAndUse(fn.Signature, fn, "function signature")
	g.signature(fn.Signature)
	g.instructions(fn)
	for _, anon := range fn.AnonFuncs {
		// (9567) functions use anonymous functions defined beneath them
		g.seeAndUse(anon, fn, "anonymous function")
		g.function(anon)
	}
}

func (g *Graph) typ(t types.Type) {
	if g.seenTypes.At(t) != nil {
		return
	}
	if t, ok := t.(*types.Named); ok && t.Obj().Pkg() != nil {
		if t.Obj().Pkg() != g.pkg.Pkg {
			return
		}
	}
	g.seenTypes.Set(t, struct{}{})
	if isIrrelevant(t) {
		return
	}

	g.see(t)
	switch t := t.(type) {
	case *types.Struct:
		for i := 0; i < t.NumFields(); i++ {
			g.see(t.Field(i))
			if t.Field(i).Exported() {
				// (2701) structs use exported fields
				g.use(t.Field(i), t, "exported struct field")
			} else if isNoCopyType(t.Field(i).Type()) {
				// (4946) structs use fields of type NoCopy sentinel
				g.use(t.Field(i), t, "NoCopy sentinel")
			}
			if t.Field(i).Anonymous() {
				// does the embedded field contribute exported methods to the method set?
				ms := g.msCache.MethodSet(t.Field(i).Type())
				for j := 0; j < ms.Len(); j++ {
					if ms.At(j).Obj().Exported() {
						// (6090) structs use embedded fields that have exported methods (recursively)
						g.use(t.Field(i), t, "extends exported method set")
						break
					}
				}

				seen := map[*types.Struct]struct{}{}
				var hasExportedField func(t types.Type) bool
				hasExportedField = func(T types.Type) bool {
					t, ok := lintdsl.Dereference(T).Underlying().(*types.Struct)
					if !ok {
						return false
					}
					if _, ok := seen[t]; ok {
						return false
					}
					seen[t] = struct{}{}
					for i := 0; i < t.NumFields(); i++ {
						field := t.Field(i)
						if field.Exported() {
							return true
						}
						if field.Embedded() && hasExportedField(field.Type()) {
							return true
						}
					}
					return false
				}
				// does the embedded field contribute exported fields?
				if hasExportedField(t.Field(i).Type()) {
					// (8728) structs use embedded structs that have exported fields (recursively)
					g.use(t.Field(i), t, "extends exported fields")
				}

			}
			g.variable(t.Field(i))
		}
	case *types.Basic:
		// Nothing to do
	case *types.Named:
		// (6108) types use their underlying and element types
		g.seeAndUse(t.Underlying(), t, "underlying type")
		g.seeAndUse(t.Obj(), t, "type name")
		g.seeAndUse(t, t.Obj(), "named type")

		for i := 0; i < t.NumMethods(); i++ {
			meth := g.pkg.Prog.FuncValue(t.Method(i))
			g.see(meth)
			if meth.Object() != nil && meth.Object().Exported() {
				// (9728) named types use exported methods
				g.use(meth, t, "exported method")
			}
			g.function(meth)
		}

		g.typ(t.Underlying())
	case *types.Slice:
		// (6108) types use their underlying and element types
		g.seeAndUse(t.Elem(), t, "element type")
		g.typ(t.Elem())
	case *types.Map:
		// (6108) types use their underlying and element types
		g.seeAndUse(t.Elem(), t, "element type")
		// (6108) types use their underlying and element types
		g.seeAndUse(t.Key(), t, "key type")
		g.typ(t.Elem())
		g.typ(t.Key())
	case *types.Signature:
		g.signature(t)
	case *types.Interface:
		for i := 0; i < t.NumMethods(); i++ {
			m := t.Method(i)
			// (3393) interfaces use all their methods. we really have no idea what is going on with interfaces.
			g.seeAndUse(m, t, "interface method")
			g.seeAndUse(m.Type().(*types.Signature), m, "signature")
			g.signature(m.Type().(*types.Signature))
		}
	case *types.Array:
		// (6108) types use their underlying and element types
		g.seeAndUse(t.Elem(), t, "element type")
		g.typ(t.Elem())
	case *types.Pointer:
		// (6108) types use their underlying and element types
		g.seeAndUse(t.Elem(), t, "element type")
		g.typ(t.Elem())
	case *types.Chan:
		// (6108) types use their underlying and element types
		g.seeAndUse(t.Elem(), t, "element type")
		g.typ(t.Elem())
	case *types.Tuple:
		for i := 0; i < t.Len(); i++ {
			// (6108) types use their underlying and element types
			g.seeAndUse(t.At(i), t, "tuple element")
			g.variable(t.At(i))
		}
	default:
		panic(fmt.Sprintf("unreachable: %T", t))
	}
}

func (g *Graph) variable(v *types.Var) {
	// (5749) variables use their types
	g.seeAndUse(v.Type(), v, "variable type")
	g.typ(v.Type())
}

func (g *Graph) signature(sig *types.Signature) {
	if sig.Recv() != nil {
		g.seeAndUse(sig.Recv(), sig, "receiver")
		g.variable(sig.Recv())
	}
	for i := 0; i < sig.Params().Len(); i++ {
		param := sig.Params().At(i)
		g.seeAndUse(param, sig, "function argument")
		g.variable(param)
	}
	for i := 0; i < sig.Results().Len(); i++ {
		param := sig.Results().At(i)
		g.seeAndUse(param, sig, "function result")
		g.variable(param)
	}
}

func (g *Graph) instructions(fn *ssa.Function) {
	if _, ok := g.seenFns[fn]; ok {
		return
	}
	g.seenFns[fn] = struct{}{}

	for _, b := range fn.Blocks {
		for _, instr := range b.Instrs {
			if v, ok := instr.(ssa.Value); ok {
				if _, ok := v.(*ssa.Range); !ok {
					// See https://github.com/golang/go/issues/19670

					// (254) instructions use their types
					g.seeAndUse(v.Type(), fn, "instruction")
					g.typ(v.Type())
				}
			}
			switch instr := instr.(type) {
			case *ssa.Field:
				st := instr.X.Type().Underlying().(*types.Struct)
				field := st.Field(instr.Field)
				// (1323) functions use fields they access
				g.seeAndUse(field, fn, "field access")
			case *ssa.FieldAddr:
				st := lintdsl.Dereference(instr.X.Type()).Underlying().(*types.Struct)
				field := st.Field(instr.Field)
				// (1323) functions use fields they access
				g.seeAndUse(field, fn, "field access")
			case *ssa.Store:
			case *ssa.Call:
				c := instr.Common()
				if !c.IsInvoke() {
					seen := map[ssa.Value]struct{}{}
					var useCall func(v ssa.Value)
					useCall = func(v ssa.Value) {
						if _, ok := seen[v]; ok {
							return
						}
						seen[v] = struct{}{}
						switch v := v.(type) {
						case *ssa.Function:
							// (9681) functions use functions/interface methods they call
							g.seeAndUse(v, fn, "function call")
							if obj := v.Object(); obj != nil {
								if cfn := g.pkg.Prog.FuncValue(obj.(*types.Func)); cfn != v {
									// The called function is a thunk (or similar),
									// process its instructions to get the call to the real function.
									// Alternatively, we could mark the function as used by the thunk.
									//
									// We can detect the thunk because ssa.Function -> types.Object -> ssa.Function
									// leads from the thunk to the real function.
									g.instructions(v)
								}
							}
						case *ssa.MakeClosure:
							useCall(v.Fn)
						case *ssa.Builtin:
							// nothing to do
						case *ssa.Phi:
							for _, e := range v.Edges {
								useCall(e)
							}
						}
					}
					// non-interface call
					useCall(c.Value)
				} else {
					// (9681) functions use functions/interface methods they call
					g.seeAndUse(c.Method, fn, "interface call")
				}
			case *ssa.Return:
				seen := map[ssa.Value]struct{}{}
				var handleReturn func(v ssa.Value)
				handleReturn = func(v ssa.Value) {
					if _, ok := seen[v]; ok {
						return
					}
					seen[v] = struct{}{}
					switch v := v.(type) {
					case *ssa.Function:
						// (8103) functions use functions they return. we assume that someone else will call the returned function
						g.seeAndUse(v, fn, "returning function")
					case *ssa.MakeClosure:
						// nothing to do. 8103 doesn't apply because this case is covered by 2521.
					case *ssa.Phi:
						for _, e := range v.Edges {
							handleReturn(e)
						}
					}
				}
				for _, v := range instr.Results {
					handleReturn(v)
				}
			case *ssa.ChangeType:
				// (853) conversions use the type they convert to
				g.seeAndUse(instr.Type(), fn, "conversion")
				g.typ(instr.Type())

				s1, ok1 := lintdsl.Dereference(instr.Type()).Underlying().(*types.Struct)
				s2, ok2 := lintdsl.Dereference(instr.X.Type()).Underlying().(*types.Struct)
				if ok1 && ok2 {
					// Converting between two structs. The fields are
					// relevant for the conversion, but only if the
					// fields are also used outside of the conversion.
					// Mark fields as used by each other.

					assert(s1.NumFields() == s2.NumFields())
					for i := 0; i < s1.NumFields(); i++ {
						g.see(s1.Field(i))
						g.see(s2.Field(i))
						// (6885) when converting between two equivalent structs, the fields in
						// either struct use each other. the fields are relevant for the
						// conversion, but only if the fields are also accessed outside the
						// conversion.
						g.seeAndUse(s1.Field(i), s2.Field(i), "struct conversion")
						g.seeAndUse(s2.Field(i), s1.Field(i), "struct conversion")
					}
				}
			case *ssa.MakeInterface:
			case *ssa.Slice:
			case *ssa.RunDefers:
				// XXX use deferred functions
			case *ssa.Convert:
				// to unsafe.Pointer
				if typ, ok := instr.Type().(*types.Basic); ok && typ.Kind() == types.UnsafePointer {
					if ptr, ok := instr.X.Type().Underlying().(*types.Pointer); ok {
						if st, ok := ptr.Elem().Underlying().(*types.Struct); ok {
							for i := 0; i < st.NumFields(); i++ {
								// (4029) when converting to or from unsafe.Pointer, mark all fields as used.
								g.seeAndUse(st.Field(i), fn, "unsafe conversion")
							}
						}
					}
				}
				// from unsafe.Pointer
				if typ, ok := instr.X.Type().(*types.Basic); ok && typ.Kind() == types.UnsafePointer {
					if ptr, ok := instr.Type().Underlying().(*types.Pointer); ok {
						if st, ok := ptr.Elem().Underlying().(*types.Struct); ok {
							for i := 0; i < st.NumFields(); i++ {
								// (4029) when converting to or from unsafe.Pointer, mark all fields as used.
								g.seeAndUse(st.Field(i), fn, "unsafe conversion")
							}
						}
					}
				}
			case *ssa.TypeAssert:
				g.seeAndUse(instr.AssertedType, fn, "type assert")
				g.typ(instr.AssertedType)
			case *ssa.MakeClosure:
				// (2521) functions use closures and bound methods.
				g.seeAndUse(instr.Fn, fn, "make closure")
				v := instr.Fn.(*ssa.Function)
				if obj := v.Object(); obj != nil {
					if cfn := g.pkg.Prog.FuncValue(obj.(*types.Func)); cfn != v {
						// The called function is a $bound (or similar),
						// process its instructions to get the call to the real function.
						// Alternatively, we could mark the function as used by the $bound.
						//
						// We can detect the $bound because ssa.Function -> types.Object -> ssa.Function
						// leads from the thunk to the real function.
						g.instructions(v)
					}
				}
			case *ssa.Alloc:
				// nothing to do
			case *ssa.UnOp:
				// nothing to do
			case *ssa.BinOp:
				// nothing to do
			case *ssa.If:
				// nothing to do
			case *ssa.Jump:
				// nothing to do
			case *ssa.IndexAddr:
				// nothing to do
			case *ssa.Extract:
				// nothing to do
			case *ssa.Panic:
				// nothing to do
			case *ssa.DebugRef:
				// nothing to do
			case *ssa.BlankStore:
				// nothing to do
			case *ssa.Phi:
				// nothing to do
			case *ssa.MakeMap:
				// nothing to do
			case *ssa.MapUpdate:
				// nothing to do
			case *ssa.Lookup:
				// nothing to do
			case *ssa.MakeSlice:
				// nothing to do
			case *ssa.Send:
				// nothing to do
			case *ssa.MakeChan:
				// nothing to do
			case *ssa.Range:
				// nothing to do
			case *ssa.Next:
				// nothing to do
			case *ssa.Index:
				// nothing to do
			case *ssa.Select:
				// nothing to do
			case *ssa.ChangeInterface:
				// XXX
			case *ssa.Go:
				// XXX
			case *ssa.Defer:
				// XXX
			default:
				panic(fmt.Sprintf("unreachable: %T", instr))
			}
		}
	}
}

// isNoCopyType reports whether a type represents the NoCopy sentinel
// type. The NoCopy type is a named struct with no fields and exactly
// one method `func Lock()` that is empty.
//
// FIXME(dh): currently we're not checking that the function body is
// empty.
func isNoCopyType(typ types.Type) bool {
	st, ok := typ.Underlying().(*types.Struct)
	if !ok {
		return false
	}
	if st.NumFields() != 0 {
		return false
	}

	named, ok := typ.(*types.Named)
	if !ok {
		return false
	}
	if named.NumMethods() != 1 {
		return false
	}
	meth := named.Method(0)
	if meth.Name() != "Lock" {
		return false
	}
	sig := meth.Type().(*types.Signature)
	if sig.Params().Len() != 0 || sig.Results().Len() != 0 {
		return false
	}
	return true
}