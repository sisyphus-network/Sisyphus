package runtime

// Kind is what sort of value a form field takes.
type Kind string

const (
	// KindFile is a stored file, given as its CID.
	KindFile Kind = "file"
	// KindInteger is a whole number.
	KindInteger Kind = "integer"
	// KindNumber is any number.
	KindNumber Kind = "number"
	// KindText is a line of text.
	KindText Kind = "text"
	// KindChoice is one of the field's choices.
	KindChoice Kind = "choice"
	// KindToggle is true or false.
	KindToggle Kind = "toggle"
)

// Field is one parameter of a workload, as a form asks for it.
type Field struct {
	// Name is the parameter's key in the job's JSON parameters.
	Name string
	Kind Kind
	// Label names the field in a few words, and Help says in a sentence
	// what it does.
	Label string
	Help  string
	// Default is the value the field starts with, or nil for none. A
	// form that sends it unchanged sends a job the workload accepts.
	Default any
	// Minimum and Maximum, if set, bound a number. Step, if not zero, is
	// what a number must be a multiple of.
	Minimum *float64
	Maximum *float64
	Step    float64
	// Choices are the values a choice field may take.
	Choices []string
	// Required is whether the workload refuses a job without it.
	Required bool
}

// Formed is a workload that can say what its parameters are as a form, so
// that someone can be asked for them field by field instead of writing the
// JSON themselves.
type Formed interface {
	Form() []Field
}

// Form returns the form of the workload with the given name, or nothing if
// it has none or is not there.
func (r *Registry) Form(name string) []Field {
	if f, ok := r.workloads[name].(Formed); ok {
		return f.Form()
	}
	return nil
}

// bound returns a bound of a field.
func bound(v float64) *float64 { return &v }
