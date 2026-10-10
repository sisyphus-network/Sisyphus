// Package sealedtest holds a blob sealed in the form used before keys were
// derived, when a blob was sealed with a sealing key itself. Nothing seals
// that way any more, so tests of what still opens such blobs need one that
// was made then.
package sealedtest

// OldKey is the sealing key, as text, that OldBlob was sealed with, and
// OldText what it holds.
const (
	OldKey  = "c2lzeXBodXMtb2xkLWZvcm1hdC10ZXN0LWtleS0wMDE"
	OldText = "sealed before each file had a key of its own\n"
)

// OldBlob is a sealed blob in the old form, in base64. It was made by this
// repository's sealed.Encrypt as it stood before the commit that added
// this file.
const OldBlob = "U0lTWUVOQzEy76bpUmFsA+OAPrD4tpbl2Q9wD9ZDtA2yRg9ga1AZkvgPrPUmjSgU+Hp5g2fMVboz4FySWEoIbVJdVKcaOYdfXSHqOQHbsHOg"
