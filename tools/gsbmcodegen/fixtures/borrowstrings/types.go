// Package borrowstrings exercises the //gsbm:borrow-strings opt-in.
package borrowstrings

// Label is a defined string type so the fixture covers named string aliases.
type Label string

//gsbm:root
type PlainRecord struct {
	ID     string            `bin:"1"`
	Note   *string           `bin:"2"`
	Names  []string          `bin:"3"`
	Labels []Label           `bin:"4"`
	Tags   map[string]string `bin:"5"`
}

//gsbm:root
//gsbm:borrow-strings
type BorrowRecord struct {
	ID     string            `bin:"1"`
	Note   *string           `bin:"2"`
	Names  []string          `bin:"3"`
	Labels []Label           `bin:"4"`
	Tags   map[string]string `bin:"5"`
}
