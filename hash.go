package gps

import (
	"crypto/sha256"
	"fmt"
	"sort"
)

// HashInputs computes a hash digest of all data in SolveParams and the
// RootManifest that act as function inputs to Solve().
//
// The digest returned from this function is the same as the digest that would
// be included with a Solve() Result. As such, it's appropriate for comparison
// against the digest stored in a lock file, generated by a previous Solve(): if
// the digests match, then manifest and lock are in sync, and a Solve() is
// unnecessary.
//
// (Basically, this is for memoization.)
func (s *solver) HashInputs() ([]byte, error) {
	// Do these checks up front before any other work is needed, as they're the
	// only things that can cause errors
	// Pass in magic root values, and the bridge will analyze the right thing
	ptree, err := s.b.listPackages(ProjectIdentifier{ProjectRoot: s.params.ImportRoot}, nil)
	if err != nil {
		return nil, badOptsFailure(fmt.Sprintf("Error while parsing packages under %s: %s", s.params.RootDir, err.Error()))
	}

	c, tc := s.rm.DependencyConstraints(), s.rm.TestDependencyConstraints()
	// Apply overrides to the constraints from the root. Otherwise, the hash
	// would be computed on the basis of a constraint from root that doesn't
	// actually affect solving.
	p := s.ovr.overrideAll(pcSliceToMap(c, tc).asSortedSlice())

	// We have everything we need; now, compute the hash.
	h := sha256.New()
	for _, pd := range p {
		h.Write([]byte(pd.Ident.ProjectRoot))
		h.Write([]byte(pd.Ident.NetworkName))
		// FIXME Constraint.String() is a surjective-only transformation - tags
		// and branches with the same name are written out as the same string.
		// This could, albeit rarely, result in input collisions when a real
		// change has occurred.
		h.Write([]byte(pd.Constraint.String()))
	}

	// The stdlib and old appengine packages play the same functional role in
	// solving as ignores. Because they change, albeit quite infrequently, we
	// have to include them in the hash.
	h.Write([]byte(stdlibPkgs))
	h.Write([]byte(appenginePkgs))

	// Write each of the packages, or the errors that were found for a
	// particular subpath, into the hash.
	for _, perr := range ptree.Packages {
		if perr.Err != nil {
			h.Write([]byte(perr.Err.Error()))
		} else {
			h.Write([]byte(perr.P.Name))
			h.Write([]byte(perr.P.CommentPath))
			h.Write([]byte(perr.P.ImportPath))
			for _, imp := range perr.P.Imports {
				h.Write([]byte(imp))
			}
			for _, imp := range perr.P.TestImports {
				h.Write([]byte(imp))
			}
		}
	}

	// Add the package ignores, if any.
	if len(s.ig) > 0 {
		// Dump and sort the ignores
		ig := make([]string, len(s.ig))
		k := 0
		for pkg := range s.ig {
			ig[k] = pkg
			k++
		}
		sort.Strings(ig)

		for _, igp := range ig {
			h.Write([]byte(igp))
		}
	}

	for _, pc := range s.ovr.asSortedSlice() {
		h.Write([]byte(pc.Ident.ProjectRoot))
		if pc.Ident.NetworkName != "" {
			h.Write([]byte(pc.Ident.NetworkName))
		}
		if pc.Constraint != nil {
			h.Write([]byte(pc.Constraint.String()))
		}
	}

	an, av := s.b.analyzerInfo()
	h.Write([]byte(an))
	h.Write([]byte(av.String()))

	return h.Sum(nil), nil
}
