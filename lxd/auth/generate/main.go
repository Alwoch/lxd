// This program reads an OpenFGA model in DSL format and generates a go file containing a type definition for
// `Entitlement`, an Entitlement each relation in the model that can has a `group#member` as a directly related user
// type, and a map of entity type to list of entitlements that can be granted for that entity type.
//
// This program expects to be run in the parent directory (lxd/auth).
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/canonical/lxd/shared/entity"
	"github.com/canonical/lxd/shared/logger"
)

var typeRegexp = regexp.MustCompile(`^type (\w+)$`)
var relationRegexp = regexp.MustCompile(`^\s+define\s+(\w+):\s+.+$`)
var commentRegexp = regexp.MustCompile(`^\s*#\s*(.*)$`)

type entitlement struct {
	relation    string
	description string
}

// snakeToPascal converts a snake case (hello_world) string to a Pascal case string (HelloWorld).
func snakeToPascal(str string) string {
	// Capitalise first letter.
	capitalise := true
	var b strings.Builder
	for _, r := range str {
		if capitalise {
			b.WriteRune(unicode.ToUpper(r))
			capitalise = false
			continue
		}

		// Capitalise letters that occur after underscores (and skip the underscores).
		if r == '_' {
			capitalise = true
			continue
		}

		b.WriteRune(r)
	}

	// Map of expected incorrect pascal case acronyms to the correct casing.
	knownAcronyms := map[string]string{
		"Acl":  "ACL",
		"Sftp": "SFTP",
	}

	s := b.String()
	for wrong, right := range knownAcronyms {
		s = strings.Replace(s, wrong, right, -1)
	}

	return s
}

func main() {
	err := func() error {
		f, err := os.Open("drivers/driver_openfga_model.openfga")
		if err != nil {
			return fmt.Errorf("Failed to open OpenFGA model file: %w", err)
		}

		defer f.Close()

		entityToEntitlements, allEntitlements, err := scanOpenFGAModel(f)
		if err != nil {
			return err
		}

		err = f.Close()
		if err != nil {
			return fmt.Errorf("Failed to close OpenFGA model file: %w", err)
		}

		outfile, err := os.Create("entitlements_generated.go")
		if err != nil {
			return fmt.Errorf("Failed to open output file: %w", err)
		}

		defer outfile.Close()

		err = writeOutput(outfile, entityToEntitlements, allEntitlements)
		if err != nil {
			return err
		}

		err = outfile.Close()
		if err != nil {
			return fmt.Errorf("Failed to close output file: %w", err)
		}

		return nil
	}()
	if err != nil {
		fmt.Printf("Failed to generate entitlements from OpenFGA model: %v\n", err)
		os.Exit(1)
	}
}

// writeOutput writes generated golang code to the given io.Writer.
func writeOutput(w io.Writer, entityToEntitlements map[entity.Type][]entitlement, allEntitlements []entitlement) error {
	var builder strings.Builder

	// Comment that the file is generated.
	builder.WriteString("// Code generated by `make update-auth`. DO NOT EDIT.\n\n")
	builder.WriteString("package auth\n\n")

	// Imports.
	builder.WriteString("import (\n")
	builder.WriteString("\t\"github.com/canonical/lxd/shared/entity\"\n")
	builder.WriteString(")\n\n")

	// Entitlement type definition.
	builder.WriteString("// Entitlement is a representation of the relations that group members can have with entity types.\n")
	builder.WriteString("type Entitlement string\n\n")

	// Write a list of all entitlements.
	builder.WriteString("const (\n")
	for i, entitlement := range allEntitlements {
		// For each entitlement, get the entity types that it can be applied to (for use in comment).
		var entityTypes []string
		for entityType, entitlements := range entityToEntitlements {
			for _, e := range entitlements {
				if entitlement.relation == e.relation {
					entityTypes = append(entityTypes, string(entityType))
					break
				}
			}
		}

		for i := range entityTypes {
			entityTypes[i] = fmt.Sprintf("entity.Type%s", snakeToPascal(entityTypes[i]))
		}

		sort.Strings(entityTypes)

		builder.WriteString(fmt.Sprintf("\t// Entitlement%s is the \"%s\" entitlement. It applies to the following entities: %s.\n", snakeToPascal(entitlement.relation), entitlement.relation, strings.Join(entityTypes, ", ")))

		if i == len(allEntitlements)-1 {
			builder.WriteString(fmt.Sprintf("\tEntitlement%s Entitlement = \"%s\"\n", snakeToPascal(entitlement.relation), entitlement.relation))
		} else {
			builder.WriteString(fmt.Sprintf("\tEntitlement%s Entitlement = \"%s\"\n\n", snakeToPascal(entitlement.relation), entitlement.relation))
		}
	}

	builder.WriteString(")\n\n")

	// To ensure the entity to entitlement map is always in the same order, get a list of entity types and sort it alphabetically.
	var entityTypes []string
	for entityType := range entityToEntitlements {
		entityTypes = append(entityTypes, string(entityType))
	}

	sort.Strings(entityTypes)

	// Map of entity.Type to slice of entitlements.
	builder.WriteString("var EntityTypeToEntitlements = map[entity.Type][]Entitlement{\n")
	for _, entityType := range entityTypes {
		entitlements := entityToEntitlements[entity.Type(entityType)]
		builder.WriteString(fmt.Sprintf("\tentity.Type%s: {\n", snakeToPascal(entityType)))
		for _, entitlement := range entitlements {
			// Here we can add the comment from the OpenFGA model.
			builder.WriteString(fmt.Sprintf("\t\t// %s\n", entitlement.description))
			builder.WriteString(fmt.Sprintf("\t\tEntitlement%s,\n", snakeToPascal(entitlement.relation)))
		}

		builder.WriteString("\t},\n")
	}

	builder.WriteString("}\n")

	// In the context of the OpenFGA model, the term "group" clearly means a collection of identities. In LXD, the term
	// "group" could have many meanings so we don't have an `entity.TypeGroup`, instead we have `entity.TypeAuthGroup`.
	// The Pascal cased "group" type will have led to adding `entity.TypeGroup` to the generated file erroneously, so we
	// need to replace it with `entity.TypeAuthGroup`.
	s := strings.Replace(builder.String(), "entity.TypeGroup", "entity.TypeAuthGroup", -1)

	_, err := w.Write([]byte(s))
	if err != nil {
		return fmt.Errorf("Failed to write output: %w", err)
	}

	return nil
}

// scanOpenFGAModel scans each line of the OpenFGA model DSL and uses regular expressions to extract types and
// relations on those types that can be considered entitlements (e.g. those that can be granted to a member of a group).
// We are using regular expressions for this instead of parsing the model so that we can also extract comments.
// Comments are not included when parsing the model with the `openfga/language` package.
func scanOpenFGAModel(r io.Reader) (map[entity.Type][]entitlement, []entitlement, error) {
	scanner := bufio.NewScanner(r)

	// A map of entity types to the entitlements that can be applied to them.
	entityToEntitlements := map[entity.Type][]entitlement{}

	// A list of all entitlements.
	var allEntitlements []entitlement

	// The current entity type.
	var curType entity.Type

	// Multiline comment, one element per line.
	var curComment []string

scan:
	for scanner.Scan() {
		line := scanner.Text()

		// Check if this is a type definition and if so, set the current type to this value.
		submatch := typeRegexp.FindStringSubmatch(line)
		if len(submatch) == 2 {
			curType = entity.Type(submatch[1])
			err := curType.Validate()
			if err != nil {
				logger.Warn("Entity type not defined for OpenFGA model type", logger.Ctx{"model_type": submatch[1], "error": err})
				continue scan
			}

			curComment = nil
			continue scan
		}

		// Check if this is a relation that can be applied to a group and if so, add it to our map/slice along with any comments we've collected.
		submatch = relationRegexp.FindStringSubmatch(line)
		if len(submatch) == 2 && strings.Contains(line, "group#member") {
			if curComment == nil {
				return nil, nil, fmt.Errorf("Entitlement %q does not have a comment", submatch[1])
			}

			entitlement := entitlement{
				relation:    submatch[1],
				description: strings.Join(curComment, " "),
			}

			entityToEntitlements[curType] = append(entityToEntitlements[curType], entitlement)

			var found bool
			for _, e := range allEntitlements {
				if submatch[1] == e.relation {
					found = true
					break
				}
			}

			if !found {
				allEntitlements = append(allEntitlements, entitlement)
			}

			curComment = nil
			continue scan
		}

		// Check if this is a comment. Append it to the current comment slice.
		submatch = commentRegexp.FindStringSubmatch(line)
		if len(submatch) == 2 {
			curComment = append(curComment, submatch[1])
		}
	}

	return entityToEntitlements, allEntitlements, nil
}
