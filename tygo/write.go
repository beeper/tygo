package tygo

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/token"
	"log"
	"regexp"
	"strconv"
	"strings"

	"github.com/fatih/structtag"
)

var (
	validJSNameRegexp     = regexp.MustCompile(`(?m)^[\pL_][\pL\pN_]*$`)
	backquoteEscapeRegexp = regexp.MustCompile(`([$\\])`)
	octalPrefixRegexp     = regexp.MustCompile(`^0[0-7]`)
	unicode8Regexp        = regexp.MustCompile(`\\\\|\\U[\da-fA-F]{8}`)
)

// https://developer.mozilla.org/en-US/docs/Web/JavaScript/Reference/Operators/Operator_precedence#table
var jsNumberOperatorPrecedence = map[token.Token]int{
	token.MUL:     6,
	token.QUO:     6,
	token.REM:     6,
	token.ADD:     5,
	token.SUB:     5,
	token.SHL:     4,
	token.SHR:     4,
	token.AND:     3,
	token.AND_NOT: 3,
	token.OR:      2,
	token.XOR:     1,
}

func validJSName(n string) bool {
	return validJSNameRegexp.MatchString(n)
}

func getIdent(s string) string {
	switch s {
	case "bool":
		return "boolean"
	case "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64",
		"float32", "float64",
		"complex64", "complex128",
		"rune":
		return "number /* " + s + " */"
	}
	return s
}

func escapeStringForEmit(str string) string {
	if strings.HasPrefix(str, "`") {
		return backquoteEscapeRegexp.ReplaceAllString(str, `\$1`)
	} else {
		return unicode8Regexp.ReplaceAllStringFunc(str, func(s string) string {
			if len(s) == 10 {
				s = fmt.Sprintf("\\u{%s}", strings.ToUpper(s[2:]))
			}
			return s
		})
	}
}

func stringifyTrivial(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.SelectorExpr:
		return fmt.Sprintf("%s.%s", t.X, t.Sel)
	case *ast.Ident:
		return t.Name
	default:
		return ""
	}
}

func (g *PackageGenerator) writeIndent(s *strings.Builder, depth int) {
	for i := 0; i < depth; i++ {
		s.WriteString(g.conf.Indent)
	}
}

func (g *PackageGenerator) writeType(
	s *strings.Builder,
	t ast.Expr,
	p ast.Expr,
	depth int,
	optionalParens bool,
) {
	// log.Println("writeType:", reflect.TypeOf(t), t)
	switch t := t.(type) {
	case *ast.StarExpr:
		if optionalParens {
			s.WriteByte('(')
		}
		g.writeType(s, t.X, t, depth, false)
		s.WriteString(" | undefined")
		if optionalParens {
			s.WriteByte(')')
		}
	case *ast.ArrayType:
		if v, ok := t.Elt.(*ast.Ident); ok && v.String() == "byte" {
			s.WriteString("string")
			break
		}
		g.writeType(s, t.Elt, t, depth, true)
		s.WriteString("[]")
	case *ast.StructType:
		s.WriteString("{\n")
		g.writeStructFields(s, t.Fields.List, depth+1)
		g.writeIndent(s, depth+1)
		s.WriteByte('}')
	case *ast.Ident:
		ts := t.String()
		// NOTE(fork): Look up type mappings even for plain identifiers. Upstream
		// only looks up mappings for selector expressions (the form `X.Y`).
		if mappedTsType, ok := g.conf.TypeMappings[ts]; ok {
			s.WriteString(mappedTsType)
		} else {
			if ts == "any" {
				s.WriteString(getIdent(g.conf.FallbackType))
			} else {
				s.WriteString(getIdent(ts))
			}
		}
	case *ast.SelectorExpr:
		// e.g. `time.Time`
		longType := stringifyTrivial(t)
		mappedTsType, ok := g.conf.TypeMappings[longType]
		if ok {
			s.WriteString(mappedTsType)
		} else { // For unknown types we use the fallback type
			s.WriteString(g.conf.FallbackType)
			s.WriteString(" /* ")
			s.WriteString(longType)
			s.WriteString(" */")
		}
	case *ast.MapType:
		s.WriteString("{ [key: ")
		// NOTE(fork): Go permits the use of `struct`s as `map` keys. JavaScript
		// offers no comparable convenience. As an escape hatch, recognize a set
		// of type names to never use as TypeScript index signature keys.
		ts := stringifyTrivial(t.Key)
		_, forbidden := g.conf.TypesForbiddenAsIndexSignatureKey[ts]
		if forbidden {
			overriddenType := "string"
			log.Printf("fork: forbidding the use of %v as an index signature key, emitting %v instead", ts, overriddenType)
			s.WriteString(overriddenType)
		} else {
			g.writeType(s, t.Key, t, depth, false)
		}
		s.WriteString("]: ")
		g.writeType(s, t.Value, t, depth, false)
		s.WriteByte('}')
	case *ast.BasicLit:
		switch t.Kind {
		case token.INT:
			if octalPrefixRegexp.MatchString(t.Value) {
				t.Value = "0o" + t.Value[1:]
			}
		case token.CHAR:
			var char rune
			if strings.HasPrefix(t.Value, `'\x`) ||
				strings.HasPrefix(t.Value, `'\u`) ||
				strings.HasPrefix(t.Value, `'\U`) {
				i32, err := strconv.ParseInt(t.Value[3:len(t.Value)-1], 16, 32)
				if err != nil {
					panic(err)
				}
				char = rune(i32)
			} else {
				var data []byte
				data = append(data, '"')
				data = append(data, []byte(t.Value[1:len(t.Value)-1])...)
				data = append(data, '"')
				var s string
				err := json.Unmarshal(data, &s)
				if err != nil {
					panic(err)
				}
				char = []rune(s)[0]
			}
			if char > 0xFFFF {
				t.Value = fmt.Sprintf("0x%08X /* %s */", char, t.Value)
			} else {
				t.Value = fmt.Sprintf("0x%04X /* %s */", char, t.Value)
			}
		case token.STRING:
			t.Value = escapeStringForEmit(t.Value)
		}
		s.WriteString(t.Value)
	case *ast.ParenExpr:
		s.WriteByte('(')
		g.writeType(s, t.X, t, depth, false)
		s.WriteByte(')')
	case *ast.BinaryExpr:
		inParen := false
		switch p := p.(type) {
		case *ast.BinaryExpr:
			if jsNumberOperatorPrecedence[t.Op] < jsNumberOperatorPrecedence[p.Op] {
				inParen = true
			}
		}
		if inParen {
			s.WriteByte('(')
		}
		g.writeType(s, t.X, t, depth, false)
		s.WriteByte(' ')
		if t.Op == token.AND_NOT {
			s.WriteString("& ~")
		} else {
			s.WriteString(t.Op.String())
			s.WriteByte(' ')
		}
		g.writeType(s, t.Y, t, depth, false)
		if inParen {
			s.WriteByte(')')
		}
	case *ast.InterfaceType:
		g.writeInterfaceFields(s, t.Methods.List, depth+1)
	case *ast.CallExpr:
		// NOTE(fork): This method is invoked for top-level constant _values_, not just
		// types. For example, we'd find ourselves here when processing:
		//
		//   const VanillaID = types.IceCreamFlavorID("vanilla")
		//
		// However, it's not appropriate to emit a type in value position, so do
		// a gross hack to recognize calls of the form:
		//
		//   X.Y("string literal")
		//
		// And ensure they make it into the output.
		if selector, ok := t.Fun.(*ast.SelectorExpr); ok && len(t.Args) == 1 {
			if lit, ok := t.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				log.Printf("fork: detected trivial use of type alias, emitting literal constant: %v (%v)", lit.Value, selector)
				s.WriteString(escapeStringForEmit(lit.Value))
				break
			}
		}
		s.WriteString(g.conf.FallbackType)
	case *ast.FuncType, *ast.ChanType:
		s.WriteString(g.conf.FallbackType)
	case *ast.UnaryExpr:
		switch t.Op {
		case token.TILDE:
			// We just ignore the tilde token, in Typescript extended types are
			// put into the generic typing itself, which we can't support yet.
			g.writeType(s, t.X, t, depth, false)
		case token.XOR:
			s.WriteString("~")
			g.writeType(s, t.X, t, depth, false)
		case token.ADD, token.SUB, token.NOT:
			s.WriteString(t.Op.String())
			g.writeType(s, t.X, t, depth, false)
		default:
			err := fmt.Errorf("unhandled unary expr: %v\n %T", t, t)
			fmt.Println(err)
			panic(err)
		}
	case *ast.IndexListExpr:
		g.writeType(s, t.X, t, depth, false)
		s.WriteByte('<')
		for i, index := range t.Indices {
			g.writeType(s, index, t, depth, false)
			if i != len(t.Indices)-1 {
				s.WriteString(", ")
			}
		}
		s.WriteByte('>')
	case *ast.IndexExpr:
		g.writeType(s, t.X, t, depth, false)
		s.WriteByte('<')
		g.writeType(s, t.Index, t, depth, false)
		s.WriteByte('>')
	default:
		err := fmt.Errorf("unhandled: %s\n %T", t, t)
		fmt.Println(err)
		panic(err)
	}
}

func (g *PackageGenerator) writeTypeParamsFields(s *strings.Builder, fields []*ast.Field) {
	s.WriteByte('<')
	for i, f := range fields {
		for j, ident := range f.Names {
			s.WriteString(ident.Name)
			s.WriteString(" extends ")
			g.writeType(s, f.Type, nil, 0, true)

			if i != len(fields)-1 || j != len(f.Names)-1 {
				s.WriteString(", ")
			}
		}
	}
	s.WriteByte('>')
}

func (g *PackageGenerator) writeInterfaceFields(
	s *strings.Builder,
	fields []*ast.Field,
	depth int,
) {
	// Usually interfaces in Golang don't have fields, but generic (union) interfaces we can map to Typescript.

	if len(fields) == 0 { // Type without any fields (probably only has methods)
		s.WriteString(g.conf.FallbackType)
		return
	}

	didContainNonFuncFields := false
	for _, f := range fields {
		if _, isFunc := f.Type.(*ast.FuncType); isFunc {
			continue
		}
		if didContainNonFuncFields {
			s.WriteString(" &\n")
		} else {
			s.WriteByte(
				'\n',
			) // We need to write a newline so comments of generic components render nicely.
			didContainNonFuncFields = true
		}

		if g.PreserveTypeComments() {
			g.writeCommentGroupIfNotNil(s, f.Doc, depth+1)
		}
		g.writeIndent(s, depth+1)
		g.writeType(s, f.Type, nil, depth, false)

		if f.Comment != nil && g.PreserveTypeComments() {
			s.WriteString(" // ")
			s.WriteString(f.Comment.Text())
		}
	}

	if !didContainNonFuncFields {
		s.WriteString(g.conf.FallbackType)
	}
}

func (g *PackageGenerator) writeStructFields(s *strings.Builder, fields []*ast.Field, depth int) {
	for _, f := range fields {
		// fmt.Println(f.Type)
		optional := false
		required := false
		readonly := false

		fieldNames := make([]string, 0, len(f.Names))
		if len(f.Names) == 0 { // anonymous field
			if name, valid := getAnonymousFieldName(f.Type); valid {
				fieldNames = append(fieldNames, name)
			}
		} else {
			for _, name := range f.Names {
				if len(name.Name) == 0 || 'A' > name.Name[0] || name.Name[0] > 'Z' {
					continue
				}
				fieldNames = append(fieldNames, name.Name)
			}
		}

		for _, fieldName := range fieldNames {

			var name string
			var tstype string
			if f.Tag != nil {
				tags, err := structtag.Parse(f.Tag.Value[1 : len(f.Tag.Value)-1])
				if err != nil {
					panic(err)
				}

				jsonTag, err := tags.Get("json")
				if err == nil {
					name = jsonTag.Name
					if name == "-" {
						continue
					}

					optional = jsonTag.HasOption("omitempty") || jsonTag.HasOption("omitzero")
				}
				yamlTag, err := tags.Get("yaml")
				if err == nil {
					name = yamlTag.Name
					if name == "-" {
						continue
					}

					optional = yamlTag.HasOption("omitempty")
				}

				tstypeTag, err := tags.Get("tstype")
				if err == nil {
					tstype = tstypeTag.Name
					if tstype == "-" || tstypeTag.HasOption("extends") {
						continue
					}
					required = tstypeTag.HasOption("required")
					readonly = tstypeTag.HasOption("readonly")
				}
			}

			if len(name) == 0 {
				if g.conf.Flavor == "yaml" {
					name = strings.ToLower(fieldName)
				} else {
					name = fieldName
				}
			}

			if g.PreserveTypeComments() {
				g.writeCommentGroupIfNotNil(s, f.Doc, depth+1)
			}

			g.writeIndent(s, depth+1)
			quoted := !validJSName(name)
			if quoted {
				s.WriteByte('\'')
			}
			if readonly {
				s.WriteString("readonly ")
			}
			s.WriteString(name)
			if quoted {
				s.WriteByte('\'')
			}

			switch t := f.Type.(type) {
			case *ast.StarExpr:
				optional = !required
				f.Type = t.X
			}

			if optional && g.conf.OptionalType == "undefined" {
				s.WriteByte('?')
			}

			s.WriteString(": ")

			if tstype == "" {
				g.writeType(s, f.Type, nil, depth, false)
				if optional && g.conf.OptionalType == "null" {
					s.WriteString(" | null")
				}
			} else {
				s.WriteString(tstype)
			}
			s.WriteByte(';')

			if f.Comment != nil && g.PreserveTypeComments() {
				// Line comment is present, that means a comment after the field.
				s.WriteString(" // ")
				s.WriteString(f.Comment.Text())
			} else {
				s.WriteByte('\n')
			}

		}

	}
}
