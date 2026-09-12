package scheduler

import (
	"slices"
	"strconv"
	"strings"
)

type fieldSpec struct {
	min   int
	max   int
	names map[string]int
}

var monthNames = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

var weekdayNames = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

type Field struct {
	values  []int
	starred bool
}

func (f Field) contains(value int) bool {
	_, found := slices.BinarySearch(f.values, value)
	return found
}

func (f Field) nextAtOrAfter(value int) (int, bool) {
	index, found := slices.BinarySearch(f.values, value)
	if found {
		return f.values[index], true
	}
	if index < len(f.values) {
		return f.values[index], true
	}
	return 0, false
}

type Schedule struct {
	Minute     Field
	Hour       Field
	DayOfMonth Field
	Month      Field
	DayOfWeek  Field
}

func ParseExpression(raw string) (Schedule, error) {
	fields := strings.Fields(raw)
	if len(fields) != 5 {
		return Schedule{}, Errorf(ErrorCodeExpressionInvalid, "expression must have five fields")
	}
	specs := []fieldSpec{
		{min: 0, max: 59},
		{min: 0, max: 23},
		{min: 1, max: 31},
		{min: 1, max: 12, names: monthNames},
		{min: 0, max: 7, names: weekdayNames},
	}
	parsed := make([]Field, 0, 5)
	for i, field := range fields {
		value, err := parseField(field, specs[i])
		if err != nil {
			return Schedule{}, err
		}
		parsed = append(parsed, value)
	}
	if sunday, ok := normalizeSunday(parsed[4]); ok {
		parsed[4] = sunday
	}
	return Schedule{
		Minute:     parsed[0],
		Hour:       parsed[1],
		DayOfMonth: parsed[2],
		Month:      parsed[3],
		DayOfWeek:  parsed[4],
	}, nil
}

func normalizeSunday(field Field) (Field, bool) {
	if !field.contains(7) {
		return field, false
	}
	values := make([]int, 0, len(field.values))
	for _, value := range field.values {
		if value == 7 {
			if !slices.Contains(values, 0) {
				values = append(values, 0)
			}
			continue
		}
		values = append(values, value)
	}
	slices.Sort(values)
	return Field{values: values, starred: field.starred}, true
}

func parseField(raw string, spec fieldSpec) (Field, error) {
	if raw == "" {
		return Field{}, Errorf(ErrorCodeExpressionInvalid, "field must not be empty")
	}
	starred := false
	values := map[int]bool{}
	for _, part := range strings.Split(raw, ",") {
		if part == "" {
			return Field{}, Errorf(ErrorCodeExpressionInvalid, "field %q has an empty element", raw)
		}
		step := 1
		span := part
		if index := strings.Index(part, "/"); index >= 0 {
			span = part[:index]
			var err error
			step, err = parseNumber(part[index+1:], 1, spec.max-spec.min+1)
			if err != nil || step <= 0 {
				return Field{}, Errorf(ErrorCodeExpressionInvalid, "field %q has an invalid step", raw)
			}
		}
		low, high := spec.min, spec.max
		switch span {
		case "", "*":
			starred = starred || span == "*"
		default:
			bounds := strings.SplitN(span, "-", 2)
			var err error
			low, err = parseValue(bounds[0], spec)
			if err != nil {
				return Field{}, err
			}
			high = low
			if len(bounds) == 2 {
				high, err = parseValue(bounds[1], spec)
				if err != nil {
					return Field{}, err
				}
			}
			if low > high {
				return Field{}, Errorf(ErrorCodeExpressionInvalid, "field %q range is reversed", raw)
			}
			if low < spec.min || high > spec.max {
				return Field{}, Errorf(ErrorCodeExpressionInvalid, "field %q is out of range", raw)
			}
		}
		for value := low; value <= high; value += step {
			values[value] = true
		}
	}
	if len(values) == 0 {
		return Field{}, Errorf(ErrorCodeExpressionInvalid, "field %q selects nothing", raw)
	}
	sorted := make([]int, 0, len(values))
	for value := range values {
		sorted = append(sorted, value)
	}
	slices.Sort(sorted)
	return Field{values: sorted, starred: starred}, nil
}

func parseValue(raw string, spec fieldSpec) (int, error) {
	if name, ok := spec.names[strings.ToLower(raw)]; ok {
		return name, nil
	}
	return parseNumber(raw, spec.min, spec.max)
}

func parseNumber(raw string, low, high int) (int, error) {
	if raw == "" {
		return 0, Errorf(ErrorCodeExpressionInvalid, "number must not be empty")
	}
	for i := range len(raw) {
		if raw[i] < '0' || raw[i] > '9' {
			return 0, Errorf(ErrorCodeExpressionInvalid, "value %q is not a number", raw)
		}
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < low || value > high {
		return 0, Errorf(ErrorCodeExpressionInvalid, "value %q is out of range", raw)
	}
	return value, nil
}

func dayMatches(schedule *Schedule, day, month, weekday int) bool {
	monthHit := schedule.Month.contains(month)
	domHit := schedule.DayOfMonth.contains(day)
	dowHit := schedule.DayOfWeek.contains(weekday)
	if !monthHit {
		return false
	}
	switch {
	case schedule.DayOfMonth.starred && schedule.DayOfWeek.starred:
		return true
	case schedule.DayOfMonth.starred:
		return dowHit
	case schedule.DayOfWeek.starred:
		return domHit
	default:
		return domHit || dowHit
	}
}
