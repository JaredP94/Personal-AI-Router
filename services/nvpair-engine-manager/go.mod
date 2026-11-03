module nvpair-engine-manager

go 1.25.0

require (
	github.com/stretchr/testify v1.11.1
	nvpair-shared v0.0.0-00010101000000-000000000000
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace nvpair-shared => ../shared

require (
	github.com/Microsoft/go-winio v0.6.2 // indirect
	golang.org/x/sys v0.47.0
)
