package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"strconv"
	"strings"
)

type nametype struct {
	name  string
	_type string
}

type constuctor struct {
	id        string
	predicate string
	params    []nametype
	_type     string
}

func normalize(s string) string {
	x := []byte(s)
	for i, r := range x {
		if r == '.' {
			x[i] = '_'
		}
	}
	y := string(x)
	if y == "type" {
		return "_type"
	}
	return y
}

func normalizeAttr(s string) string {
	s = strings.Replace(s, "_", " ", -1)
	s = strings.Title(s)
	s = strings.Replace(s, " ", "", -1)
	if strings.HasSuffix(s, "Id") {
		s = s[:len(s)-2] + "ID"
	}
	return s
}

func maybeFlagged(_type string, isFlag bool, flagBit int) string {
	if isFlag {
		return fmt.Sprintf("m.Flagged%s(flags, %d),\n", _type, flagBit)
	} else {
		return fmt.Sprintf("m.%s(),\n", _type)
	}
}

func main() {
	if len(os.Args) != 3 {
		println("Usage: " + os.Args[0] + " tl_schema.json tl_schema.go")
		os.Exit(2)
	}

	// reading json file
	data, err := ioutil.ReadFile(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}

	// opening out file
	outFile, err := os.Create(os.Args[2])
	if err != nil {
		log.Fatal(err)
	}
	defer outFile.Close()
	write := func(format string, a ...interface{}) {
		if _, err := fmt.Fprintf(outFile, format, a...); err != nil {
			log.Fatal(err)
		}
	}

	// parse json
	var parsed interface{}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := d.Decode(&parsed); err != nil {
		log.Fatal(err)
	}

	// process constructors
	_order := make([]string, 0, 1000)
	_cons := make(map[string]constuctor, 1000)
	_types := make(map[string][]string, 1000)

	parsefunc := func(data []interface{}, kind string) {
		for _, data := range data {
			data := data.(map[string]interface{})

			// id
			idx, err := strconv.Atoi(data["id"].(string))
			if err != nil {
				log.Fatal(err)
			}
			_id := fmt.Sprintf("0x%08x", uint32(idx))

			// predicate
			_predicate := normalize(data[kind].(string))

			if _predicate == "vector" {
				continue
			}

			// params
			_params := make([]nametype, 0, 16)
			params := data["params"].([]interface{})
			for _, params := range params {
				params := params.(map[string]interface{})
				_params = append(_params, nametype{normalize(params["name"].(string)), normalize(params["type"].(string))})
			}

			// type
			_type := normalize(data["type"].(string))

			_order = append(_order, _predicate)
			_cons[_predicate] = constuctor{_id, _predicate, _params, _type}
			if kind == "predicate" {
				_types[_type] = append(_types[_type], _predicate)
			}
		}
	}
	parsefunc(parsed.(map[string]interface{})["constructors"].([]interface{}), "predicate")
	parsefunc(parsed.(map[string]interface{})["methods"].([]interface{}), "method")

	// constants
	write("package mtproto\nimport \"fmt\"\nconst (\n")
	for _, key := range _order {
		c := _cons[key]
		write("CRC_%s = %s\n", c.predicate, c.id)
	}
	write(")\n\n")

	// type structs
	for _, key := range _order {
		c := _cons[key]
		write("type TL_%s struct {\n", c.predicate)
		for _, t := range c.params {
			isFlag := false
			typeName := t._type
			if strings.HasPrefix(t._type, "flags") {
				isFlag = true
				typeName = t._type[strings.Index(t._type, "?")+1:]
			}
			write("%s\t", normalizeAttr(t.name))
			switch typeName {
			case "true": //flags only
				write("bool")
			case "int", "#":
				write("int32")
			case "long":
				write("int64")
			case "string":
				write("string")
			case "double":
				write("float64")
			case "bytes":
				write("[]byte")
			case "Vector<int>":
				write("[]int32")
			case "Vector<long>":
				write("[]int64")
			case "Vector<string>":
				write("[]string")
			case "Vector<double>":
				write("[]float64")
			case "!X":
				write("TL")
			default:
				var inner string
				n, _ := fmt.Sscanf(typeName, "Vector<%s", &inner)
				if n == 1 {
					write("[]TL // %s", inner[:len(inner)-1])
				} else {
					write("TL // %s", typeName)
				}
			}
			if isFlag {
				write(" //flag")
			}
			write("\n")
		}
		write("}\n\n")
	}

	// encode funcs
	for _, key := range _order {
		c := _cons[key]
		write("func (e TL_%s) encode() []byte {\n", c.predicate)
		write("x := NewEncodeBuf(512)\n")
		write("x.UInt(CRC_%s)\n", c.predicate)
		for _, t := range c.params {
			isFlag := false
			typeName := t._type
			flagBit := 0
			if strings.HasPrefix(t._type, "flags") {
				isFlag = true
				typeName = t._type[strings.Index(t._type, "?")+1:]
				flagBit, _ = strconv.Atoi(string(t._type[strings.Index(t._type, "_")+1 : strings.Index(t._type, "?")]))
			}
			attrName := normalizeAttr(t.name)
			if isFlag && typeName != "true" {
				write("if e.Flags & %d != 0 {\n", 1<<uint(flagBit))
			}
			switch typeName {
			case "true": //flags only
				write("//flag %s\n", attrName)
			case "int", "#":
				write("x.Int(e.%s)\n", attrName)
			case "long":
				write("x.Long(e.%s)\n", attrName)
			case "string":
				write("x.String(e.%s)\n", attrName)
			case "double":
				write("x.Double(e.%s)\n", attrName)
			case "bytes":
				write("x.StringBytes(e.%s)\n", attrName)
			case "Vector<int>":
				write("x.VectorInt(e.%s)\n", attrName)
			case "Vector<long>":
				write("x.VectorLong(e.%s)\n", attrName)
			case "Vector<string>":
				write("x.VectorString(e.%s)\n", attrName)
			case "Vector<double>":
				write("x.VectorDouble(e.%s)\n", attrName)
			case "!X":
				write("x.Bytes(e.%s.encode())\n", attrName)
			default:
				var inner string
				n, _ := fmt.Sscanf(typeName, "Vector<%s", &inner)
				if n == 1 {
					write("x.Vector(e.%s)\n", attrName)
				} else {
					write("x.Bytes(e.%s.encode())\n", attrName)
				}
			}
			if isFlag && typeName != "true" {
				write("}\n")
			}
		}
		write("return x.buf\n")
		write("}\n\n")
	}

	// decode funcs
	write(`
func (m *DecodeBuf) ObjectGenerated(constructor uint32) (r TL) {
	switch constructor {`)

	for _, key := range _order {
		c := _cons[key]
		write("case CRC_%s:\n", c.predicate)
		for _, t := range c.params {
			if t._type == "#" {
				write("flags := m.Int()\n")
				break
			}
		}
		write("r = TL_%s{\n", c.predicate)
		for _, t := range c.params {
			isFlag := false
			flagBit := 0
			typeName := t._type
			if strings.HasPrefix(t._type, "flags") {
				isFlag = true
				flagBit, _ = strconv.Atoi(string(t._type[strings.Index(t._type, "_")+1 : strings.Index(t._type, "?")]))
				typeName = t._type[strings.Index(t._type, "?")+1:]
			}
			switch typeName {
			case "true": //flags only
				write("false, //flag\n")
			case "#":
				write("flags,\n")
			case "int":
				write(maybeFlagged("Int", isFlag, flagBit))
			case "long":
				write(maybeFlagged("Long", isFlag, flagBit))
			case "string":
				write(maybeFlagged("String", isFlag, flagBit))
			case "double":
				write(maybeFlagged("Double", isFlag, flagBit))
			case "bytes":
				write(maybeFlagged("StringBytes", isFlag, flagBit))
			case "Vector<int>":
				write(maybeFlagged("VectorInt", isFlag, flagBit))
			case "Vector<long>":
				write(maybeFlagged("VectorLong", isFlag, flagBit))
			case "Vector<string>":
				write(maybeFlagged("VectorString", isFlag, flagBit))
			case "Vector<double>":
				write(maybeFlagged("VectorDouble", isFlag, flagBit))
			case "!X":
				write(maybeFlagged("Object", isFlag, flagBit))
			default:
				var inner string
				n, _ := fmt.Sscanf(typeName, "Vector<%s", &inner)
				if n == 1 {
					write(maybeFlagged("Vector", isFlag, flagBit))
				} else {
					write(maybeFlagged("Object", isFlag, flagBit))
				}
			}
		}
		write("}\n\n")
	}

	write(`
	default:
		m.err = fmt.Errorf("Unknown constructor: \u002508x", constructor)
		return nil

	}

	if m.err != nil {
		return nil
	}

	return
}`)
}
