package app

type Applier interface {
	Apply(applyMsg interface{})
	Name() string
}
