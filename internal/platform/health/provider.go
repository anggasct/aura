package health

type HealthProvider interface {
	HealthChecks() []RegisteredCheck
}
