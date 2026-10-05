package jobmodel

import "sort"

// The coordinator tells a job which stored blobs its work touched; the job
// sorts them into what it consumed, what it produced, and what only passed
// between its own steps.

// NoteRead records blobs the job opened.
func (j *Job) NoteRead(cids ...string) { add(j.read, cids) }

// NoteTaskOutput records blobs a task stored.
func (j *Job) NoteTaskOutput(cids ...string) { add(j.intermediate, cids) }

// NoteResult records blobs the aggregation stored, which make up the result.
func (j *Job) NoteResult(cids ...string) { add(j.outputs, cids) }

// InputBlobs returns the blobs the job consumed: those it opened but did not
// itself produce.
func (j *Job) InputBlobs() []string {
	var inputs []string
	for c := range j.read {
		_, mid := j.intermediate[c]
		_, out := j.outputs[c]
		if !mid && !out {
			inputs = append(inputs, c)
		}
	}
	sort.Strings(inputs)
	return inputs
}

// OutputBlobs returns the blobs the job's result consists of.
func (j *Job) OutputBlobs() []string {
	return sorted(j.outputs)
}

// IntermediateBlobs returns the blobs that only passed between the job's
// tasks and its aggregation, and are of no use once it has finished. A blob
// a task stored that is also an input or part of the result is not one.
func (j *Job) IntermediateBlobs() []string {
	var mid []string
	for c := range j.intermediate {
		if _, out := j.outputs[c]; !out {
			mid = append(mid, c)
		}
	}
	sort.Strings(mid)
	return mid
}

func add(set map[string]struct{}, cids []string) {
	for _, c := range cids {
		set[c] = struct{}{}
	}
}

func sorted(set map[string]struct{}) []string {
	list := make([]string, 0, len(set))
	for c := range set {
		list = append(list, c)
	}
	sort.Strings(list)
	return list
}
