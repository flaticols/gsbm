package api

import "example.com/modulefixture/internal/inner"

//gsbm:root
type Order struct {
	ID  inner.ID  `bin:"1"`
	Tag inner.Tag `bin:"2"`
}
