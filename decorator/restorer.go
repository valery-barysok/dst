package decorator

import (
	"fmt"
	"go/ast"
	"go/format"
	"go/token"
	"io"
	"os"
	"strings"

	"github.com/dave/dst"
)

// Print uses format.Node to print a *dst.File to stdout
func Print(f *dst.File) error {
	return Fprint(os.Stdout, f)
}

// Fprint uses format.Node to print a *dst.File to a writer
func Fprint(w io.Writer, f *dst.File) error {
	fset, af := Restore(f)
	return format.Node(w, fset, af)
}

// Restore restores a *dst.File to a *token.FileSet and a *ast.File
func Restore(file *dst.File) (*token.FileSet, *ast.File) {
	r := NewRestorer()
	return r.Fset, r.RestoreFile("", file)
}

// NewRestorer creates a new Restorer
func NewRestorer() *Restorer {
	return &Restorer{
		Fset: token.NewFileSet(),
		Map: Map{
			Ast: AstMap{
				Nodes:   map[dst.Node]ast.Node{},
				Scopes:  map[*dst.Scope]*ast.Scope{},
				Objects: map[*dst.Object]*ast.Object{},
			},
			Dst: DstMap{
				Nodes:   map[ast.Node]dst.Node{},
				Scopes:  map[*ast.Scope]*dst.Scope{},
				Objects: map[*ast.Object]*dst.Object{},
			},
		},
	}
}

type Restorer struct {
	Map
	Fset *token.FileSet // Fset is the *token.FileSet in use. Set this to use a pre-existing FileSet.
}

type Map struct {
	Ast AstMap
	Dst DstMap
}

type AstMap struct {
	Nodes   map[dst.Node]ast.Node       // Mapping from dst to ast Nodes
	Objects map[*dst.Object]*ast.Object // Mapping from dst to ast Objects
	Scopes  map[*dst.Scope]*ast.Scope   // Mapping from dst to ast Scopes
}

type DstMap struct {
	Nodes   map[ast.Node]dst.Node       // Mapping from ast to dst Nodes
	Objects map[*ast.Object]*dst.Object // Mapping from ast to dst Objects
	Scopes  map[*ast.Scope]*dst.Scope   // Mapping from ast to dst Scopes
}

type fileRestorer struct {
	*Restorer
	lines           []int
	comments        []*ast.CommentGroup
	base            int
	cursor          token.Pos
	nodeDecl        map[*ast.Object]dst.Node // Objects that have a ast.Node Decl (look up after file has been rendered)
	nodeData        map[*ast.Object]dst.Node // Objects that have a ast.Node Data (look up after file has been rendered)
	cursorAtNewLine token.Pos                // The cursor position directly after adding a newline decoration (or a line comment which ends in a "\n"). If we're still at this cursor position when we add a line space, reduce the "\n" by one.
}

// RestoreFile restores a *dst.File to an *ast.File
func (r *Restorer) RestoreFile(name string, file *dst.File) *ast.File {

	// Base is the pos that the file will start at in the fset
	base := r.Fset.Base()

	fr := &fileRestorer{
		Restorer: r,
		lines:    []int{0}, // initialise with the first line at Pos 0
		base:     base,
		cursor:   token.Pos(base),
		nodeDecl: map[*ast.Object]dst.Node{},
		nodeData: map[*ast.Object]dst.Node{},
	}

	// restore the file, populate comments and lines
	f := fr.restoreNode(file, false).(*ast.File)

	for _, cg := range fr.comments {
		f.Comments = append(f.Comments, cg)
	}

	size := fr.fileSize()

	ff := r.Fset.AddFile(name, base, size)
	if !ff.SetLines(fr.lines) {
		panic("SetLines failed")
	}

	// Sometimes new nodes are created here (e.g. in RangeStmt the "Object" is an AssignStmt which
	// never occurs in the actual code). These shouldn't have position information but perhaps it
	// doesn't matter?
	// TODO: Disable all position information on these nodes?
	for o, dn := range fr.nodeDecl {
		o.Decl = fr.restoreNode(dn, true)
	}
	for o, dn := range fr.nodeData {
		o.Data = fr.restoreNode(dn, true)
	}

	return f
}

func (f *fileRestorer) fileSize() int {

	// If a comment is at the end of a file, it will extend past the current cursor position...

	end := int(f.cursor) // end pos of file

	// check that none of the comments or newlines extend past the file end position. If so, increment.
	for _, cg := range f.comments {
		if int(cg.End()) >= end {
			end = int(cg.End()) + 1
		}
	}
	for _, lineOffset := range f.lines {
		pos := lineOffset + f.base // remember lines are relative to the file base
		if pos >= end {
			end = pos + 1
		}
	}

	return end - f.base
}

func (f *fileRestorer) applyLiteral(text string) {
	isMultiLine := strings.HasPrefix(text, "`") && strings.Contains(text, "\n")
	if !isMultiLine {
		return
	}
	for charIndex, char := range text {
		if char == '\n' {
			lineOffset := int(f.cursor) - f.base + charIndex // remember lines are relative to the file base
			f.lines = append(f.lines, lineOffset)
		}
	}
}

func (f *fileRestorer) hasCommentField(n ast.Node) bool {
	switch n.(type) {
	case *ast.Field, *ast.ValueSpec, *ast.TypeSpec, *ast.ImportSpec:
		return true
	}
	return false
}

func (f *fileRestorer) addCommentField(n ast.Node, slash token.Pos, text string) {
	c := &ast.Comment{Slash: slash, Text: text}
	switch n := n.(type) {
	case *ast.Field:
		if n.Comment == nil {
			n.Comment = &ast.CommentGroup{}
			f.comments = append(f.comments, n.Comment)
		}
		n.Comment.List = append(n.Comment.List, c)
	case *ast.ImportSpec:
		if n.Comment == nil {
			n.Comment = &ast.CommentGroup{}
			f.comments = append(f.comments, n.Comment)
		}
		n.Comment.List = append(n.Comment.List, c)
	case *ast.ValueSpec:
		if n.Comment == nil {
			n.Comment = &ast.CommentGroup{}
			f.comments = append(f.comments, n.Comment)
		}
		n.Comment.List = append(n.Comment.List, c)
	case *ast.TypeSpec:
		if n.Comment == nil {
			n.Comment = &ast.CommentGroup{}
			f.comments = append(f.comments, n.Comment)
		}
		n.Comment.List = append(n.Comment.List, c)
	}
}

func (f *fileRestorer) applyDecorations(node ast.Node, decorations dst.Decorations, end bool) {
	firstLine := true
	for _, d := range decorations {

		isNewline := d == "\n"
		isLineComment := strings.HasPrefix(d, "//")
		isInlineComment := strings.HasPrefix(d, "/*")
		isComment := isLineComment || isInlineComment
		isMultiLineComment := isInlineComment && strings.Contains(d, "\n")

		if end && f.cursorAtNewLine == f.cursor {
			f.cursor++ // indent all comments in "End" decorations
		}

		// for multi-line comments, add a newline for each \n
		if isMultiLineComment {
			for charIndex, char := range d {
				if char == '\n' {
					lineOffset := int(f.cursor) - f.base + charIndex // remember lines are relative to the file base
					f.lines = append(f.lines, lineOffset)
				}
			}
		}

		// if the decoration is a comment, add it and advance the cursor
		if isComment {
			if firstLine && end && f.hasCommentField(node) {
				// for comments on the same line as the end of a node that has a Comment field, we
				// add the comment to the node instead of the file.
				f.addCommentField(node, f.cursor, d)
			} else {
				f.comments = append(f.comments, &ast.CommentGroup{List: []*ast.Comment{{Slash: f.cursor, Text: d}}})
			}
			f.cursor += token.Pos(len(d))
		}

		// for newline decorations and also line-comments, add a newline
		if isLineComment || isNewline {
			lineOffset := int(f.cursor) - f.base // remember lines are relative to the file base
			f.lines = append(f.lines, lineOffset)
			f.cursor++

			f.cursorAtNewLine = f.cursor
		}

		if isNewline || isLineComment {
			firstLine = false
		}
	}
}

func (f *fileRestorer) applySpace(space dst.SpaceType) {
	var newlines int
	switch space {
	case dst.NewLine:
		newlines = 1
	case dst.EmptyLine:
		newlines = 2
	}
	if f.cursor == f.cursorAtNewLine {
		newlines--
	}
	for i := 0; i < newlines; i++ {

		// Advance the cursor one more byte for all newlines, so we step over any required
		// separator char - e.g. comma. See net-hook test
		f.cursor++

		lineOffset := int(f.cursor) - f.base // remember lines are relative to the file base
		f.lines = append(f.lines, lineOffset)
		f.cursor++
		f.cursorAtNewLine = f.cursor
	}
}

func (r *fileRestorer) restoreObject(o *dst.Object) *ast.Object {
	if o == nil {
		return nil
	}
	if ro, ok := r.Ast.Objects[o]; ok {
		return ro
	}
	/*
		// An Object describes a named language entity such as a package,
		// constant, type, variable, function (incl. methods), or label.
		//
		// The Data fields contains object-specific data:
		//
		//	Kind    Data type         Data value
		//	Pkg     *Scope            package scope
		//	Con     int               iota for the respective declaration
		//
		type Object struct {
			Kind ObjKind
			Name string      // declared name
			Decl interface{} // corresponding Field, XxxSpec, FuncDecl, LabeledStmt, AssignStmt, Scope; or nil
			Data interface{} // object-specific data; or nil
			Type interface{} // placeholder for type information; may be nil
		}
	*/
	out := &ast.Object{}

	r.Ast.Objects[o] = out
	r.Dst.Objects[out] = o

	out.Kind = ast.ObjKind(o.Kind)
	out.Name = o.Name

	switch decl := o.Decl.(type) {
	case *dst.Scope:
		out.Decl = r.restoreScope(decl)
	case dst.Node:
		// Can't use restoreNode here because we aren't at the right cursor position, so we store a link
		// to the Object and Node so we can look the Nodes up in the cache after the file is fully processed.
		r.nodeDecl[out] = decl
	case nil:
	default:
		panic(fmt.Sprintf("o.Decl is %T", o.Decl))
	}

	// TODO: I believe Data is either a *Scope or an int. We will support both and panic if something else if found.
	switch data := o.Data.(type) {
	case int:
		out.Data = data
	case *dst.Scope:
		out.Data = r.restoreScope(data)
	case dst.Node:
		// Can't use restoreNode here because we aren't at the right cursor position, so we store a link
		// to the Object and Node so we can look the Nodes up in the cache after the file is fully processed.
		r.nodeData[out] = data
	case nil:
	default:
		panic(fmt.Sprintf("o.Data is %T", o.Data))
	}

	return out
}

func (r *fileRestorer) restoreScope(s *dst.Scope) *ast.Scope {
	if s == nil {
		return nil
	}
	if rs, ok := r.Ast.Scopes[s]; ok {
		return rs
	}
	/*
		// A Scope maintains the set of named language entities declared
		// in the scope and a link to the immediately surrounding (outer)
		// scope.
		//
		type Scope struct {
			Outer   *Scope
			Objects map[string]*Object
		}
	*/
	out := &ast.Scope{}

	r.Ast.Scopes[s] = out
	r.Dst.Scopes[out] = s

	out.Outer = r.restoreScope(s.Outer)
	out.Objects = map[string]*ast.Object{}
	for k, v := range s.Objects {
		out.Objects[k] = r.restoreObject(v)
	}

	return out
}
