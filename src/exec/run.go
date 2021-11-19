/* Jacobin VM -- A Java virtual machine
 * © Copyright 2021 by Andrew Binstock. All rights reserved
 * Licensed under Mozilla Public License 2.0 (MPL-2.0)
 */

package exec

import (
	"errors"
	"fmt"
	"jacobin/log"
	"strconv"
)

// StartExec accepts the name of the starting class, finds its main() method
// in the method area (it's guaranteed to already be loaded), grabs the executable
// bytes, creates a thread of execution, pushes the main() frame onto the JVM stack
// and begins execution.
func StartExec(className string) error {
	m, cpp, err := fetchMethodAndCP(className, "main")
	if err != nil {
		return errors.New("Class not found: " + className + ".main()")
	}

	f := frame{} // create a new frame
	f.methName = "main"
	f.clName = className
	f.cp = cpp                                  // add its pointer to the class CP
	for i := 0; i < len(m.CodeAttr.Code); i++ { // copy the bytecodes over
		f.meth = append(f.meth, m.CodeAttr.Code[i])
	}

	// allocate the operand stack
	for j := 0; j < m.CodeAttr.MaxStack; j++ {
		f.opStack = append(f.opStack, int32(0))
	}
	f.tos = -1

	// allocate the local variables
	for k := 0; k < m.CodeAttr.MaxLocals; k++ {
		f.locals = append(f.locals, 0)
	}

	// create the first thread and place its first frame on it
	t := CreateThread(0)
	f.thread = t.id
	if pushFrame(&t.stack, f) != nil {
		_ = log.Log("Memory error allocating frame on thread: "+strconv.Itoa(t.id), log.SEVERE)
	}

	err = runThread(t)
	if err != nil {
		return err
	}
	return nil
}

func runThread(t execThread) error {
	currFrame := t.stack.frames[t.stack.top]
	return runFrame(currFrame)
}

func runFrame(f frame) error {
	for pc := 0; pc < len(f.meth); pc++ {
		switch f.meth[pc] {
		case 0x02: // iconst_n1    (push -1 onto opStack)
			push(&f, -1)
		case 0x03: // iconst_0     (push 0 onto opStack)
			push(&f, 0)
		case 0x04: // iconst_1     (push 1 onto opStack)
			push(&f, 1)
		case 0x05: // iconst_2     (push 2 onto opStack)
			push(&f, 2)
		case 0x06: // iconst_3     (push 3 onto opStack)
			push(&f, 3)
		case 0x07: // iconst_4     (push 4 onto opStack)
			push(&f, 4)
		case 0x08: // iconst_5     (push 5 onto opStack)
			push(&f, 5)
		case 0x10: // bipush       push the following byte as an int onto the stack
			push(&f, int32(f.meth[pc+1]))
			pc += 1
		case 0x12: // ldc          (push constant from CP indexed by next byte)
			push(&f, int32(f.meth[pc+1]))
			pc += 1
		case 0x1A: // iload_0      (push local variable 0)
			push(&f, f.locals[0])
		case 0x1B: // iload_1      (push local variable 1)
			push(&f, f.locals[1])
		case 0x1C: // iload_2      (push local variable 2)
			push(&f, f.locals[2])
		case 0x1D: // iload_3      (push local variable 3)
			push(&f, f.locals[3])
		case 0x3B: // istore_0     (store popped top of stack int into local 0)
			f.locals[0] = pop(&f)
		case 0x3C: // istore_1     (store popped top of stack int into local 1)
			f.locals[1] = pop(&f)
		case 0x3D: // istore_2     (store popped top of stack int into local 2)
			f.locals[2] = pop(&f)
		case 0x3E: // istore_3     (store popped top of stack int into local 3)
			f.locals[3] = pop(&f)
		case 0xA2: // icmpge       (jump if popped val1 >= popped val2)
			val2 := pop(&f)
			val1 := pop(&f)
			if val1 >= val2 { // if comp succeeds, next 2 bytes hold instruction index
				jumpTo := (int(f.meth[pc+1]) * 256) + int(f.meth[pc+2])
				pc = jumpTo - 1 // -1 b/c on the next iteration, pc is bumped by 1
			} else {
				pc += 2
			}
		case 0xB2: // getstatic
			// TODO: getstatic will instantiate a static class if it's not already instantiated
			// that logic has not yet been implemented and the code here is simply a reasonable
			// placeholder, which consists of creating a struct that holds most of the needed info
			// puts it into a slice of such static fields and pushes the index of this item in the slice
			// onto the stack of the frame.
			CPslot := (int(f.meth[pc+1]) * 256) + int(f.meth[pc+2]) // next 2 bytes point to CP entry
			pc += 2
			CPentry := f.cp.CpIndex[CPslot]
			if CPentry.Type != FieldRef { // the pointed-to CP entry must be a field reference
				return fmt.Errorf("Expected a field ref on getstatic, but got %d in"+
					"location %d in method %s of class %s\n",
					CPentry.Type, pc, f.methName, f.clName)
			}
			// fmt.Fprintf(os.Stderr, "getstatic, CP entry: type %d, slot %d\n",
			// 	CPentry.Type, CPentry.Slot)

			// get the field entry
			field := f.cp.FieldRefs[CPentry.Slot]

			// get the class entry from the field entry for this field. It's the class name.
			classRef := field.ClassIndex
			classNameIndex := f.cp.ClassRefs[f.cp.CpIndex[classRef].Slot]
			classNameEntry := f.cp.CpIndex[classNameIndex]
			className := f.cp.Utf8Refs[classNameEntry.Slot]
			println("Field name: " + className)

			// process the name and type entry for this field
			nAndTindex := field.NameAndType
			nAndTentry := f.cp.CpIndex[nAndTindex]
			nAndTslot := nAndTentry.Slot
			nAndT := f.cp.NameAndTypes[nAndTslot]
			fieldNameIndex := nAndT.NameIndex
			fieldName := FetchUTF8stringFromCPEntryNumber(f.cp, fieldNameIndex)
			fieldName = className + "." + fieldName

			// was this static field previously loaded? Is so, get its location and move on.
			prevLoaded, ok := Statics[fieldName]
			if ok { // if preloaded, then push the index into the array of constant fields
				push(&f, prevLoaded)
				continue
			}

			fieldTypeIndex := nAndT.DescIndex
			fieldType := FetchUTF8stringFromCPEntryNumber(f.cp, fieldTypeIndex)
			println("full field name: " + fieldName + ", type: " + fieldType)
			newStatic := Static{
				Class:     'L',
				Type:      fieldType,
				ValueRef:  "",
				ValueInt:  0,
				ValueFP:   0,
				ValueStr:  "",
				ValueFunc: nil,
			}
			StaticsArray = append(StaticsArray, newStatic)
			Statics[fieldName] = int32(len(StaticsArray) - 1)
			// push the pointer to the stack of the frame
			push(&f, int32(len(StaticsArray)-1))

		case 0xB6: // invokevirtual (create new frame, invoke function)
			CPslot := (int(f.meth[pc+1]) * 256) + int(f.meth[pc+2]) // next 2 bytes point to CP entry
			pc += 2
			CPentry := f.cp.CpIndex[CPslot]
			if CPentry.Type != MethodRef { // the pointed-to CP entry must be a field reference
				return fmt.Errorf("Expected a method ref for invokevirtual, but got %d in"+
					"location %d in method %s of class %s\n",
					CPentry.Type, pc, f.methName, f.clName)
			}

			// get the methodRef entry
			method := f.cp.MethodRefs[CPentry.Slot]

			// get the class entry from this method
			classRef := method.ClassIndex
			classNameIndex := f.cp.ClassRefs[f.cp.CpIndex[classRef].Slot]
			classNameEntry := f.cp.CpIndex[classNameIndex]
			className := f.cp.Utf8Refs[classNameEntry.Slot]
			// println("Method class name: " + className)

			// get the method name for this method
			nAndTindex := method.NameAndType
			nAndTentry := f.cp.CpIndex[nAndTindex]
			nAndTslot := nAndTentry.Slot
			nAndT := f.cp.NameAndTypes[nAndTslot]
			methodNameIndex := nAndT.NameIndex
			methodName := FetchUTF8stringFromCPEntryNumber(f.cp, methodNameIndex)
			methodName = className + "." + methodName
			println("Method name for invokevirtual: " + methodName)

			// get the signature for this method
			methodSigIndex := nAndT.DescIndex
			methodSig := FetchUTF8stringFromCPEntryNumber(f.cp, methodSigIndex)
			println("Method for invokevirtual-name: " + methodName + ", type: " + methodSig)

		default:
			msg := fmt.Sprintf("Invalid bytecode found: %d at location %d in method %s() of class %s\n",
				f.meth[pc], pc, f.methName, f.clName)
			_ = log.Log(msg, log.SEVERE)
			return errors.New("invalid bytecode encountered")
		}
	}
	return nil
}

// pop from the operand stack. TODO: need to put in checks for invalid pops
func pop(f *frame) int32 {
	value := f.opStack[f.tos]
	f.tos -= 1
	return value
}

// push onto the operand stack
func push(f *frame, i int32) {
	f.tos += 1
	f.opStack[f.tos] = i
}