package operation

type Behavior struct {
	ID      string
	Group   string
	Summary string
}

type BehaviorProvider interface {
	Behaviors() []Behavior
}
