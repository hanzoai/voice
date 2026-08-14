package voice

import "strings"

// Sentences cuts a stream of text into utterances a synthesiser can speak
// before the rest of the text exists.
//
// This is the whole latency argument. Synthesising a reply in one call pays for
// every sentence before the first is heard; measured on kokoro that is 2267 ms
// against 452 ms for the first sentence alone. After the first, synthesis runs
// while the previous clip plays and costs nothing, because the models run
// several times faster than speech.
//
// The cut is on terminal punctuation followed by a space, which is where a
// speaker would breathe. A decimal point, an abbreviation or an ellipsis is
// not such a place, so `atEnd` refuses those; a clause that never terminates
// is cut at `most` anyway, since silence while a model runs on is worse than
// an early breath.
func Sentences(in <-chan string, most int) <-chan string {
	out := make(chan string)
	go func() {
		defer close(out)
		var held strings.Builder
		flush := func() {
			if s := strings.TrimSpace(held.String()); s != "" {
				out <- s
			}
			held.Reset()
		}
		for piece := range in {
			for _, r := range piece {
				held.WriteRune(r)
				if held.Len() >= most && r == ' ' {
					flush()
					continue
				}
				if r == ' ' && atEnd(held.String()) {
					flush()
				}
			}
		}
		flush()
	}()
	return out
}

// atEnd reports whether text ends a sentence rather than merely a token that
// happens to carry a dot.
func atEnd(text string) bool {
	t := strings.TrimRight(text, " ")
	if t == "" {
		return false
	}
	last := t[len(t)-1]
	if last != '.' && last != '!' && last != '?' {
		return false
	}
	// "..." is a pause inside a thought, not the end of one.
	if strings.HasSuffix(t, "...") {
		return false
	}
	body := t[:len(t)-1]
	if body == "" {
		return false
	}
	// A digit before the dot is a decimal or a list number: "3.14", "1. ".
	if c := body[len(body)-1]; c >= '0' && c <= '9' {
		return false
	}
	// A single letter before the dot is an initial or an abbreviation: "J.", "e.g."
	word := body
	if i := strings.LastIndexAny(body, " \t"); i >= 0 {
		word = body[i+1:]
	}
	if len(word) == 1 {
		return false
	}
	switch strings.ToLower(word) {
	case "mr", "mrs", "ms", "dr", "prof", "st", "vs", "etc", "e.g", "i.e", "no":
		return false
	}
	return true
}
