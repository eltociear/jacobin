/*
 * Jacobin VM - A Java virtual machine
 * Copyright (c) 2022-3 by the Jacobin authors. All rights reserved.
 * Licensed under Mozilla Public License 2.0 (MPL 2.0)
 */

package jvm

import (
	"container/list"
	"encoding/binary"
	"errors"
	"fmt"
	"jacobin/classloader"
	"jacobin/exceptions"
	"jacobin/frames"
	"jacobin/globals"
	"jacobin/javaTypes"
	"jacobin/log"
	"jacobin/object"
	"jacobin/shutdown"
	"jacobin/thread"
	"jacobin/util"
	"math"
	"strconv"
	"unsafe"
)

var MainThread thread.ExecThread

// StartExec is where execution begins. It initializes various structures, such as
// the MTable, then using the passed-in name of the starting class, finds its main() method
// in the method area (it's guaranteed to already be loaded), grabs the executable
// bytes, creates a thread of execution, pushes the main() frame onto the JVM stack
// and begins execution.
func StartExec(className string, globals *globals.Globals) error {
	// initialize the MTable
	classloader.MTable = make(map[string]classloader.MTentry)
	classloader.MTableLoadNatives()

	me, err := classloader.FetchMethodAndCP(className, "main", "([Ljava/lang/String;)V")
	if err != nil {
		return errors.New("Class not found: " + className + ".main()")
	}

	m := me.Meth.(classloader.JmEntry)
	f := frames.CreateFrame(m.MaxStack) // create a new frame
	f.MethName = "main"
	f.ClName = className
	f.CP = m.Cp                        // add its pointer to the class CP
	for i := 0; i < len(m.Code); i++ { // copy the bytecodes over
		f.Meth = append(f.Meth, m.Code[i])
	}

	// allocate the local variables
	for k := 0; k < m.MaxLocals; k++ {
		f.Locals = append(f.Locals, 0)
	}

	// create the first thread and place its first frame on it
	MainThread = thread.CreateThread()
	MainThread.Stack = frames.CreateFrameStack()
	MainThread.ID = thread.AddThreadToTable(&MainThread, &globals.Threads)

	tracing := false
	trace, exists := globals.Options["-trace"]
	if exists {
		tracing = trace.Set
	}
	MainThread.Trace = tracing
	f.Thread = MainThread.ID

	if frames.PushFrame(MainThread.Stack, f) != nil {
		_ = log.Log("Memory exceptions allocating frame on thread: "+strconv.Itoa(MainThread.ID), log.SEVERE)
		return errors.New("outOfMemory Exception")
	}

	if MainThread.Trace {
		traceInfo := fmt.Sprintf("StartExec: f.MethName=%s, m.MaxStack=%d, m.MaxLocals=%d, len(m.Code)=%d", f.MethName, m.MaxStack, m.MaxLocals, len(m.Code))
		_ = log.Log(traceInfo, log.TRACE_INST)
	}

	err = runThread(&MainThread)
	if err != nil {
		return err
	}
	return nil
}

// Point the thread to the top of the frame stack and tell it to run from there.
func runThread(t *thread.ExecThread) error {
	for t.Stack.Len() > 0 {
		err := runFrame(t.Stack)
		if err != nil {
			return err
		}

		if t.Stack.Len() == 1 { // true when the last executed frame was main()
			return nil
		}
	}
	return nil
}

// Convert a byte to an int64 by extending the sign-bit
func byteToInt64(bite byte) int64 {
	if (bite & 0x80) == 0x80 { // Negative bite value (left-most bit on)?
		// Negative byte - need to extend the sign (left-most) bit
		var wbytes = []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x00}
		wbytes[7] = bite
		// Form an int64 from the wbytes array
		// If you know C, this is equivalent to memcpy(&wint64, &wbytes, 8)
		return int64(binary.BigEndian.Uint64(wbytes))
	}

	// Not negative (left-most bit off) : just cast bite as an int64
	return int64(bite)
}

// runFrame() is the principal execution function in Jacobin. It first tests for a
// golang function in the present frame. If it is a golang function, it's sent to
// a different function for execution. Otherwise, bytecode interpretation takes
// place through a giant switch statement.
func runFrame(fs *list.List) error {
	// the current frame is always the head of the linked list of frames.
	// the next statement converts the address of that frame to the more readable 'f'
	f := fs.Front().Value.(*frames.Frame)

	// if the frame contains a golang method, execute it using runGframe(),
	// which returns a value (possibly nil) and an exceptions code. Presuming no exceptions,
	// if the return value (here, retval) is not nil, it is placed on the stack
	// of the calling frame.
	if f.Ftype == 'G' {
		retval, slotCount, err := runGframe(f)

		if retval != nil {
			f = fs.Front().Next().Value.(*frames.Frame)
			push(f, retval.(int64)) // if slotCount = 1

			if slotCount == 2 {
				push(f, retval.(int64)) // push a second time, if a long, double, etc.
			}
		}
		return err
	}

	// the frame's method is not a golang method, so it's Java bytecode, which
	// is interpreted in the rest of this function.
	for f.PC < len(f.Meth) {
		if MainThread.Trace {
			var tos = " -"
			var stackTop = ""
			if f.TOS != -1 {
				tos = fmt.Sprintf("%2d", f.TOS)
				stackTopValuePrt := convertInterfaceToUint64(f.OpStack[f.TOS])
				stackTop = fmt.Sprintf("0x%08X (%T)", stackTopValuePrt, f.OpStack[f.TOS])
			}

			traceInfo :=
				"class: " + fmt.Sprintf("%-10s", f.ClName) +
					" meth: " + fmt.Sprintf("%-10s", f.MethName) +
					" PC: " + fmt.Sprintf("% 3d", f.PC) +
					", " + fmt.Sprintf("%-13s", BytecodeNames[int(f.Meth[f.PC])]) +
					" TOS: " + tos +
					" " + stackTop +
					" "
			_ = log.Log(traceInfo, log.TRACE_INST)
		}
		switch f.Meth[f.PC] { // cases listed in numerical value of opcode
		case NOP:
			break
		case ACONST_NULL: // 0x01   (push null onto opStack)
			push(f, int64(0))
		case ICONST_M1: //	x02	(push -1 onto opStack)
			push(f, int64(-1))
		case ICONST_0: // 	0x03	(push int 0 onto opStack)
			push(f, int64(0))
		case ICONST_1: //  	0x04	(push int 1 onto opStack)
			push(f, int64(1))
		case ICONST_2: //   0x05	(push 2 onto opStack)
			push(f, int64(2))
		case ICONST_3: //   0x06	(push 3 onto opStack)
			push(f, int64(3))
		case ICONST_4: //   0x07	(push 4 onto opStack)
			push(f, int64(4))
		case ICONST_5: //   0x08	(push 5 onto opStack)
			push(f, int64(5))
		case LCONST_0: //   0x09    (push long 0 onto opStack)
			push(f, int64(0)) // b/c longs take two slots on the stack, it's pushed twice
			push(f, int64(0))
		case LCONST_1: //   0x0A    (push long 1 on to opStack)
			push(f, int64(1)) // b/c longs take two slots on the stack, it's pushed twice
			push(f, int64(1))
		case FCONST_0: // 0x0B
			push(f, 0.0)
		case FCONST_1: // 0x0C
			push(f, 1.0)
		case FCONST_2: // 0x0D
			push(f, 2.0)
		case DCONST_0: // 0x0E
			push(f, 0.0)
			push(f, 0.0)
		case DCONST_1: // 0xoF
			push(f, 1.0)
			push(f, 1.0)
		case BIPUSH: //	0x10	(push the following byte as an int onto the stack)
			wbyte := f.Meth[f.PC+1]
			wint64 := byteToInt64(wbyte)
			f.PC += 1
			push(f, wint64)
		case SIPUSH: //	0x11	(create int from next two bytes and push the int)
			wbyte1 := f.Meth[f.PC+1]
			wbyte2 := f.Meth[f.PC+2]
			var wint64 int64
			if (wbyte1 & 0x80) == 0x80 { // Negative wbyte1 (left-most bit on)?
				// Negative wbyte1 : form wbytes = 6 0xFFs concatenated with the wbyte1 and wbyte2
				var wbytes = []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x00, 0x00}
				wbytes[6] = wbyte1
				wbytes[7] = wbyte2
				// Form an int64 from the wbytes array
				// If you know C, this is equivalent to memcpy(&wint64, &wbytes, 8)
				wint64 = int64(binary.BigEndian.Uint64(wbytes))
			} else {
				// Not negative (left-most bit off) : just cast wbyte as an int64
				wint64 = (int64(wbyte1) * 256) + int64(wbyte2)
			}
			f.PC += 2
			push(f, wint64)
		case LDC: // 	0x12   	(push constant from CP indexed by next byte)
			idx := f.Meth[f.PC+1]
			f.PC += 1

			CPe := FetchCPentry(f.CP, int(idx))
			if CPe.entryType != 0 && // 0 = error
				// Note: an invalid CP entry causes a java.lang.Verify error and
				//       is caught before execution of the program begins.
				// This instruction does not load longs or doubles
				CPe.entryType != classloader.DoubleConst &&
				CPe.entryType != classloader.LongConst { // if no error
				if CPe.retType == IS_INT64 {
					push(f, CPe.intVal)
				} else if CPe.retType == IS_FLOAT64 {
					push(f, CPe.floatVal)
				} else if CPe.retType == IS_STRUCT_ADDR {
					push(f, unsafe.Pointer(CPe.addrVal))
				} else if CPe.retType == IS_STRING_ADDR {
					stringAddr :=
						object.CreateJavaStringFromGoString(CPe.stringVal)
					stringAddr.Klass = classloader.MethAreaFetch("java/lang/String")
					// push(f, unsafe.Pointer(CPe.addrVal))
					push(f, stringAddr)
				}
			} else { // TODO: Determine what exception to throw
				exceptions.Throw(exceptions.InaccessibleObjectException,
					"Invalid type for LDC instruction")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}
		case LDC_W: // 	0x13	(push constant from CP indexed by next two bytes)
			idx := (int(f.Meth[f.PC+1]) * 256) + int(f.Meth[f.PC+2])
			f.PC += 2

			CPe := FetchCPentry(f.CP, idx)
			if CPe.entryType != 0 && // this instruction does not load longs or doubles
				CPe.entryType != classloader.DoubleConst &&
				CPe.entryType != classloader.LongConst { // if no error
				if CPe.retType == IS_INT64 {
					push(f, CPe.intVal)
				} else if CPe.retType == IS_FLOAT64 {
					push(f, CPe.floatVal)
				} else {
					push(f, unsafe.Pointer(CPe.addrVal))
				}
			} else { // TODO: Determine what exception to throw
				exceptions.Throw(exceptions.InaccessibleObjectException, "Invalid type for LDC_W instruction")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}
		case LDC2_W: // 0x14 	(push long or double from CP indexed by next two bytes)
			idx := (int(f.Meth[f.PC+1]) * 256) + int(f.Meth[f.PC+2])
			f.PC += 2

			CPe := FetchCPentry(f.CP, idx)
			if CPe.retType == IS_INT64 { // push value twice (due to 64-bit width)
				push(f, CPe.intVal)
				push(f, CPe.intVal)
			} else if CPe.retType == IS_FLOAT64 {
				push(f, CPe.floatVal)
				push(f, CPe.floatVal)
			} else { // TODO: Determine what exception to throw
				exceptions.Throw(exceptions.InaccessibleObjectException, "Invalid type for LDC2_W instruction")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}
		case ILOAD, // 0x15	(push int from local var, using next byte as index)
			FLOAD, //  0x17 (push float from local var, using next byte as index)
			ALOAD: //  0x19 (push ref from local var, using next byte as index)
			index := int(f.Meth[f.PC+1])
			f.PC += 1
			push(f, f.Locals[index])
		case LLOAD: // 0x16 (push long from local var, using next byte as index)
			index := int(f.Meth[f.PC+1])
			f.PC += 1
			val := f.Locals[index].(int64)
			push(f, val)
			push(f, val) // push twice due to item being 64 bits wide
		case DLOAD: // 0x18 (push double from local var, using next byte as index)
			index := int(f.Meth[f.PC+1])
			f.PC += 1
			val := f.Locals[index].(float64)
			push(f, val)
			push(f, val) // push twice due to item being 64 bits wide
		case ILOAD_0: // 	0x1A    (push local variable 0)
			push(f, f.Locals[0].(int64))
		case ILOAD_1: //    OX1B    (push local variable 1)
			push(f, f.Locals[1].(int64))
		case ILOAD_2: //    0X1C    (push local variable 2)
			push(f, f.Locals[2].(int64))
		case ILOAD_3: //  	0x1D   	(push local variable 3)
			push(f, f.Locals[3].(int64))
		// LLOAD use two slots, so the same value is pushed twice
		case LLOAD_0: //	0x1E	(push local variable 0, as long)
			push(f, f.Locals[0].(int64))
			push(f, f.Locals[0].(int64))
		case LLOAD_1: //	0x1F	(push local variable 1, as long)
			push(f, f.Locals[1].(int64))
			push(f, f.Locals[1].(int64))
		case LLOAD_2: //	0x20	(push local variable 2, as long)
			push(f, f.Locals[2].(int64))
			push(f, f.Locals[2].(int64))
		case LLOAD_3: //	0x21	(push local variable 3, as long)
			push(f, f.Locals[3].(int64))
			push(f, f.Locals[3].(int64))
		case FLOAD_0: // 0x22
			push(f, f.Locals[0])
		case FLOAD_1: // 0x23
			push(f, f.Locals[1])
		case FLOAD_2: // 0x24
			push(f, f.Locals[2])
		case FLOAD_3: // 0x25
			push(f, f.Locals[3])
		case DLOAD_0: //	0x26	(push local variable 0, as double)
			push(f, f.Locals[0])
			push(f, f.Locals[0])
		case DLOAD_1: //	0x27	(push local variable 1, as double)
			push(f, f.Locals[1])
			push(f, f.Locals[1])
		case DLOAD_2: //	0x28	(push local variable 2, as double)
			push(f, f.Locals[2])
			push(f, f.Locals[2])
		case DLOAD_3: //	0x29	(push local variable 3, as double)
			push(f, f.Locals[3])
			push(f, f.Locals[3])
		case ALOAD_0: //	0x2A	(push reference stored in local variable 0)
			push(f, f.Locals[0])
		case ALOAD_1: //	0x2B	(push reference stored in local variable 1)
			push(f, f.Locals[1])
		case ALOAD_2: //	0x2C    (push reference stored in local variable 2)
			push(f, f.Locals[2])
		case ALOAD_3: //	0x2D	(push reference stored in local variable 3)
			push(f, f.Locals[3])
		case IALOAD, //		0x2E	(push contents of an int array element)
			CALOAD, //		0x34	(push contents of a (two-byte) char array element)
			SALOAD: //		0x35    (push contents of a short array element)
			index := pop(f).(int64)
			iAref := pop(f).(*object.Object) // ptr to array object
			if iAref == nil {
				exceptions.Throw(exceptions.NullPointerException, "Invalid (null) reference to an array")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}
			array := *(iAref.Fields[0].Fvalue).(*[]int64)

			if index >= int64(len(array)) {
				exceptions.Throw(exceptions.ArrayIndexOutOfBoundsException, "Invalid array subscript")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}
			var value = array[index]
			push(f, value)
		case LALOAD: //		0x2F	(push contents of a long array element)
			index := pop(f).(int64)
			iAref := pop(f).(*object.Object) // ptr to array object
			if iAref == nil {
				exceptions.Throw(exceptions.NullPointerException,
					"LALOAD: Invalid (null) reference to an array")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}

			array := *(iAref.Fields[0].Fvalue).(*[]int64)
			if index >= int64(len(array)) {
				exceptions.Throw(exceptions.ArrayIndexOutOfBoundsException,
					"LALOAD: Invalid array subscript")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}
			var value = array[index]
			push(f, value)
			push(f, value) // pushed twice due to JDK longs being 64 bits wide

		case FALOAD: //		0x30	(push contents of an float array element)
			index := pop(f).(int64)
			fAref := pop(f).(*object.Object) // ptr to array object
			// fAref := (*object.JacobinFloatArray)(ref)
			if fAref == nil {
				exceptions.Throw(exceptions.NullPointerException, "Invalid (null) reference to an array")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}

			array := *(fAref.Fields[0].Fvalue).(*[]float64)
			if index >= int64(len(array)) {
				exceptions.Throw(exceptions.ArrayIndexOutOfBoundsException, "Invalid array subscript")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}
			var value = array[index]
			push(f, value)

		case DALOAD: //		0x31	(push contents of a double array element)
			index := pop(f).(int64)
			fAref := pop(f).(*object.Object) // ptr to array object
			if fAref == nil {
				exceptions.Throw(exceptions.NullPointerException,
					"DALOAD: Invalid (null) reference to an array")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}
			array := *(fAref.Fields[0].Fvalue).(*[]float64)

			if index >= int64(len(array)) {
				exceptions.Throw(exceptions.ArrayIndexOutOfBoundsException,
					"DALOAD: Invalid array subscript")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}
			var value = array[index]
			push(f, value)
			push(f, value)
		case AALOAD: // 0x32    (push contents of a reference array element)
			index := pop(f).(int64)
			rAref := pop(f).(*object.Object) // the array object
			if rAref == nil {
				exceptions.Throw(exceptions.NullPointerException,
					"AALOAD: Invalid (null) reference to an array")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}

			arrayPtr := rAref.Fields[0].Fvalue.(*[]*object.Object)
			size := int64(len(*arrayPtr))
			if index >= size {
				exceptions.Throw(exceptions.ArrayIndexOutOfBoundsException,
					"AALOAD: Invalid array subscript")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}
			array := *(arrayPtr)
			var value = array[index]
			push(f, value)

		case BALOAD: // 0x33	(push contents of a byte/boolean array element)
			index := pop(f).(int64)
			bAref := pop(f).(*object.Object) // the array object
			if bAref == nil {
				exceptions.Throw(exceptions.NullPointerException,
					"BALOAD: Invalid (null) reference to an array")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}
			// array := *(bAref.Arr)

			arrayPtr := bAref.Fields[0].Fvalue.(*[]javaTypes.JavaByte)
			size := int64(len(*arrayPtr))

			if index >= size {
				exceptions.Throw(exceptions.ArrayIndexOutOfBoundsException,
					"BALOAD: Invalid array subscript")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}
			array := *(arrayPtr)
			var value = array[index]
			push(f, int64(value))

		case ISTORE, //  0x36 	(store popped top of stack int into local[index])
			LSTORE: //  0x37 (store popped top of stack long into local[index])
			bytecode := f.Meth[f.PC]
			index := int(f.Meth[f.PC+1])
			f.PC += 1
			f.Locals[index] = pop(f).(int64)
			// longs and doubles are stored in localvar[x] and again in localvar[x+1]
			if bytecode == LSTORE {
				f.Locals[index+1] = pop(f).(int64)
			}
		case FSTORE: //  0x38 (store popped top of stack float into local[index])
			index := int(f.Meth[f.PC+1])
			f.PC += 1
			f.Locals[index] = pop(f).(float64)
		case DSTORE: //  0x39 (store popped top of stack double into local[index])
			index := int(f.Meth[f.PC+1])
			f.PC += 1
			f.Locals[index] = pop(f).(float64)
			// longs and doubles are stored in localvar[x] and again in localvar[x+1]
			f.Locals[index+1] = pop(f).(float64)
		case ASTORE: //  0x3A (store popped top of stack ref into localc[index])
			index := int(f.Meth[f.PC+1])
			f.PC += 1
			f.Locals[index] = pop(f)
		case ISTORE_0: //   0x3B    (store popped top of stack int into local 0)
			f.Locals[0] = pop(f).(int64)
		case ISTORE_1: //   0x3C   	(store popped top of stack int into local 1)
			f.Locals[1] = pop(f).(int64)
		case ISTORE_2: //   0x3D   	(store popped top of stack int into local 2)
			f.Locals[2] = pop(f).(int64)
		case ISTORE_3: //   0x3E    (store popped top of stack int into local 3)
			f.Locals[3] = pop(f).(int64)
		case LSTORE_0: //   0x3F    (store long from top of stack into locals 0 and 1)
			var v = pop(f).(int64)
			f.Locals[0] = v
			f.Locals[1] = v
			pop(f)
		case LSTORE_1: //   0x40    (store long from top of stack into locals 1 and 2)
			var v = pop(f).(int64)
			f.Locals[1] = v
			f.Locals[2] = v
			pop(f)
		case LSTORE_2: //   0x41    (store long from top of stack into locals 2 and 3)
			var v = pop(f).(int64)
			f.Locals[2] = v
			f.Locals[3] = v
			pop(f)
		case LSTORE_3: //   0x42    (store long from top of stack into locals 3 and 4)
			var v = pop(f).(int64)
			f.Locals[3] = v
			f.Locals[4] = v
			pop(f)
		case FSTORE_0: // 0x43
			f.Locals[0] = pop(f).(float64)
		case FSTORE_1: // 0x44
			f.Locals[1] = pop(f).(float64)
		case FSTORE_2: // 0x45
			f.Locals[2] = pop(f).(float64)
		case FSTORE_3: // 0x46
			f.Locals[3] = pop(f).(float64)
		case DSTORE_0: // 0x47
			pop(f)
			f.Locals[0] = pop(f).(float64)
		case DSTORE_1: // 0x48
			pop(f)
			f.Locals[1] = pop(f).(float64)
		case DSTORE_2: // 0x49
			pop(f)
			f.Locals[2] = pop(f).(float64)
		case DSTORE_3: // 0x4A
			pop(f)
			f.Locals[3] = pop(f).(float64)
		case ASTORE_0: //	0x4B	(pop reference into local variable 0)
			f.Locals[0] = pop(f)
		case ASTORE_1: //   0x4C	(pop reference into local variable 1)
			f.Locals[1] = pop(f)
		case ASTORE_2: // 	0x4D	(pop reference into local variable 2)
			f.Locals[2] = pop(f)
		case ASTORE_3: //	0x4E	(pop reference into local variable 3)
			f.Locals[3] = pop(f)
		case IASTORE, //	0x4F	(store int in an array)
			CASTORE, //		0x55 	(store char (2 bytes) in an array)
			SASTORE: //    	0x56	(store a short in an array)
			value := pop(f).(int64)
			index := pop(f).(int64)
			arrObj := pop(f).(*object.Object) // the array object
			if arrObj == nil {
				exceptions.Throw(exceptions.NullPointerException, "Invalid (null) reference to an array")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}

			if arrObj.Fields[0].Ftype != "[I" {
				exceptions.Throw(exceptions.ArrayStoreException, "IASTORE: Attempt to access array of incorrect type")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}

			array := *(arrObj.Fields[0].Fvalue).(*[]int64)
			size := int64(len(array))
			if index >= size {
				exceptions.Throw(exceptions.ArrayIndexOutOfBoundsException, "Invalid array subscript")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}
			array[index] = value

		case LASTORE: // 0x50	(store a long in a long array)
			value := pop(f).(int64)
			pop(f) // second pop b/c longs use two slots
			index := pop(f).(int64)
			lAref := pop(f).(*object.Object) // ptr to array object
			if lAref == nil {
				exceptions.Throw(exceptions.NullPointerException, "LASTORE: Invalid (null) reference to an array")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}

			arrType := lAref.Fields[0].Ftype

			if arrType != "[I" {
				exceptions.Throw(exceptions.ArrayStoreException,
					"LASTORE: Attempt to access array of incorrect type")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}

			array := *(lAref.Fields[0].Fvalue).(*[]int64)
			size := int64(len(array))
			if index >= size {
				exceptions.Throw(exceptions.ArrayIndexOutOfBoundsException,
					"LASTORE: Invalid array subscript")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}
			array[index] = value

		case FASTORE: // 0x51	(store a float in a float array)
			value := pop(f).(float64)
			index := pop(f).(int64)
			fAref := pop(f).(*object.Object) // ptr to array object
			if fAref == nil {
				exceptions.Throw(exceptions.NullPointerException,
					"FASTORE: Invalid (null) reference to an array")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}

			if fAref.Fields[0].Ftype != "[F" {
				exceptions.Throw(exceptions.ArrayStoreException,
					"FASTORE: Attempt to access array of incorrect type")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}

			array := *(fAref.Fields[0].Fvalue).(*[]float64)
			size := int64(len(array))
			if index >= size {
				exceptions.Throw(exceptions.ArrayIndexOutOfBoundsException,
					"FASTORE: Invalid array subscript")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}
			array[index] = value

		case DASTORE: // 0x52	(store a double in a doubles array)
			value := pop(f).(float64)
			pop(f) // second pop b/c doubles take two slots on the operand stack
			index := pop(f).(int64)
			dAref := pop(f).(*object.Object)
			if dAref == nil {
				exceptions.Throw(exceptions.NullPointerException,
					"DASTORE: Invalid (null) reference to an array")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}

			if dAref.Fields[0].Ftype != "[F" {
				exceptions.Throw(exceptions.ArrayStoreException,
					"DASTORE: Attempt to access array of incorrect type")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}

			array := *(dAref.Fields[0].Fvalue).(*[]float64)
			size := int64(len(array))
			if index >= size {
				exceptions.Throw(exceptions.ArrayIndexOutOfBoundsException,
					"DASTORE: Invalid array subscript")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}

			array[index] = value

		case AASTORE: // 0x53   (store a reference in a reference array)
			value := pop(f).(*object.Object) // reference we're inserting
			index := pop(f).(int64)
			ptrObj := pop(f).(*object.Object) // ptr to the array object

			if ptrObj == nil {
				exceptions.Throw(exceptions.NullPointerException,
					"AASTORE: Invalid (null) reference to an array")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}

			// get pointer to the actual array
			arrayPtr := ptrObj.Fields[0].Fvalue.(*[]*object.Object)
			size := int64(len(*arrayPtr))
			if index >= size {
				exceptions.Throw(exceptions.ArrayIndexOutOfBoundsException,
					"AASTORE: Invalid array subscript")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}

			array := *arrayPtr
			array[index] = value

		case BASTORE: // 0x54 	(store a boolean or byte in byte array)
			var value int8 = 0
			rawValue := pop(f)
			value = convertInterfaceToInt8(rawValue)
			index := pop(f).(int64)
			ptrObj := pop(f).(*object.Object) // ptr to array object
			if ptrObj == nil {
				exceptions.Throw(exceptions.NullPointerException,
					"BASTORE: Invalid (null) reference to an array")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}

			if ptrObj.Fields[0].Ftype != "[B" {
				exceptions.Throw(exceptions.ArrayStoreException,
					"BASTORE: Attempt to access array of incorrect type")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}

			array := *(ptrObj.Fields[0].Fvalue.(*[]javaTypes.JavaByte))
			size := int64(len(array))
			if index >= size {
				exceptions.Throw(exceptions.ArrayIndexOutOfBoundsException,
					"BASTORE: Invalid array subscript")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}

			array[index] = value

		case POP: // 0x57 	(pop an item off the stack and discard it)
			pop(f)
		case POP2: // 0x58	(pop 2 itmes from stack and discard them)
			pop(f)
			pop(f)
		case DUP: // 0x59 			(push an item equal to the current top of the stack
			push(f, peek(f))
		case DUP_X1: // 0x5A		(Duplicate the top stack value and insert two values down)
			top := pop(f)
			next := pop(f)
			push(f, top)
			push(f, next)
			push(f, top)
		case DUP_X2: // 0x5B		(Duplicate top stack value and insert it three slots earlier)
			top := pop(f)
			next := pop(f)
			third := pop(f)
			push(f, top)
			push(f, third)
			push(f, next)
			push(f, top)
		case DUP2: // 0x5C			(Duplicate the top two stack values)
			top := pop(f)
			next := peek(f)
			push(f, top)
			push(f, next)
			push(f, top)
		case DUP2_X1: // 0x5D		(Duplicate the top two values, three slots down)
			top := pop(f)
			next := pop(f)
			third := pop(f)
			push(f, next) // so: top-next-third -> top-next-third->top->next
			push(f, top)
			push(f, third)
			push(f, next)
			push(f, top)
		case DUP2_X2: // 0x5E		(Duplicate the top two values, four slots down)
			top := pop(f)
			next := pop(f)
			third := pop(f)
			fourth := pop(f)
			push(f, next) // so: top-next-third-fourth -> top-next-third-fourth-top-next
			push(f, top)
			push(f, fourth)
			push(f, third)
			push(f, next)
			push(f, top)
		case SWAP: // 0x5F 	(swap top two items on stack)
			top := pop(f)
			next := pop(f)
			push(f, top)
			push(f, next)
		case IADD: //  0x60		(add top 2 integers on operand stack, push result)
			i2 := pop(f).(int64)
			i1 := pop(f).(int64)
			sum := add(i1, i2)
			push(f, sum)
		case LADD: //  0x61     (add top 2 longs on operand stack, push result)
			l2 := pop(f).(int64) //    longs occupy two slots, hence double pushes and pops
			pop(f)
			l1 := pop(f).(int64)
			pop(f)
			sum := add(l1, l2)
			push(f, sum)
			push(f, sum)
		case FADD: // 0x62
			lhs := float32(pop(f).(float64))
			rhs := float32(pop(f).(float64))
			push(f, float64(lhs+rhs))
		case DADD: // 0x63
			lhs := pop(f).(float64)
			pop(f)
			rhs := pop(f).(float64)
			pop(f)
			res := add(lhs, rhs)
			push(f, res)
			push(f, res)
		case ISUB: //  0x64	(subtract top 2 integers on operand stack, push result)
			i2 := pop(f).(int64)
			i1 := pop(f).(int64)
			diff := subtract(i1, i2)
			push(f, diff)
		case LSUB: //  0x65 (subtract top 2 longs on operand stack, push result)
			i2 := pop(f).(int64) //    longs occupy two slots, hence double pushes and pops
			pop(f)
			i1 := pop(f).(int64)
			pop(f)
			diff := subtract(i1, i2)

			push(f, diff)
			push(f, diff)
		case FSUB: // 0x66
			i2 := float32(pop(f).(float64))
			i1 := float32(pop(f).(float64))
			push(f, float64(i1-i2))
		case DSUB: // 0x67
			val2 := pop(f).(float64)
			pop(f)
			val1 := pop(f).(float64)
			pop(f)
			res := val1 - val2
			push(f, res)
			push(f, res)
		case IMUL: //  0x68  	(multiply 2 integers on operand stack, push result)
			i2 := pop(f).(int64)
			i1 := pop(f).(int64)
			product := multiply(i1, i2)

			push(f, product)
		case LMUL: //  0x69     (multiply 2 longs on operand stack, push result)
			l2 := pop(f).(int64) //    longs occupy two slots, hence double pushes and pops
			pop(f)
			l1 := pop(f).(int64)
			pop(f)
			product := multiply(l1, l2)

			push(f, product)
			push(f, product)
		case FMUL: // 0x6A
			val1 := float32(pop(f).(float64))
			val2 := float32(pop(f).(float64))
			push(f, float64(val1*val2))
		case DMUL: // 0x6B
			val1 := pop(f).(float64)
			pop(f)
			val2 := pop(f).(float64)
			pop(f)
			res := multiply(val1, val2)
			push(f, res)
			push(f, res)
		case IDIV: //  0x6C (integer divide tos-1 by tos)
			val1 := pop(f).(int64)
			if val1 == 0 {
				exceptions.Throw(exceptions.ArithmeticException, ""+
					"IDIV: Arithmetic Exception: divide by zero")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			} else {
				val2 := pop(f).(int64)
				push(f, val2/val1)
			}
		case LDIV: //  0x6D   (long divide tos-2 by tos)
			val2 := pop(f).(int64)
			pop(f) //    longs occupy two slots, hence double pushes and pops
			if val2 == 0 {
				exceptions.Throw(exceptions.ArithmeticException, ""+
					"LDIV: Arithmetic Exception: divide by zero")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			} else {
				val1 := pop(f).(int64)
				pop(f)
				res := val1 / val2
				push(f, res)
				push(f, res)
			}
		case FDIV: // 0x6E
			val1 := pop(f).(float64)
			val2 := pop(f).(float64)
			if val1 == 0.0 {
				if val2 == 0.0 {
					push(f, math.NaN())
				} else if math.Signbit(val1) {
					push(f, math.Inf(1))
				} else {
					push(f, math.Inf(-1))
				}
			} else {
				push(f, float64(float32(val2)/float32(val1)))
			}
		case DDIV: // 0x6F
			val1 := pop(f).(float64)
			pop(f)
			val2 := pop(f).(float64)
			pop(f)
			if val1 == 0.0 {
				if val2 == 0.0 {
					push(f, math.NaN())
				} else if math.Signbit(val1) {
					push(f, math.Inf(1))
				} else {
					push(f, math.Inf(-1))
				}
			} else {
				res := val2 / val1
				push(f, res)
				push(f, res)
			}
		case IREM: // 	0x70	(remainder after int division, modulo)
			val2 := pop(f).(int64)
			if val2 == 0 {
				exceptions.Throw(exceptions.ArithmeticException,
					"IREM: Arithmetic Exception: divide by zero")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			} else {
				val1 := pop(f).(int64)
				res := val1 % val2
				push(f, res)
			}
		case LREM: // 	0x71	(remainder after long division)
			val2 := pop(f).(int64)
			pop(f) //    longs occupy two slots, hence double pushes and pops
			if val2 == 0 {
				exceptions.Throw(exceptions.ArithmeticException,
					"LREM: Arithmetic Exception: divide by zero")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			} else {
				val1 := pop(f).(int64)
				pop(f)
				res := val1 % val2
				push(f, res)
				push(f, res)
			}
		case FREM: // 0x72
			val2 := pop(f).(float64)
			val1 := pop(f).(float64)
			push(f, float64(float32(math.Remainder(val1, val2))))
		case DREM: // 0x73
			val2 := pop(f).(float64)
			pop(f)
			val1 := pop(f).(float64)
			pop(f)
			push(f, math.Remainder(val1, val2))
		case INEG: //	0x74 	(negate an int)
			val := pop(f).(int64)
			push(f, -val)
		case LNEG: //   0x75	(negate a long)
			val := pop(f).(int64)
			pop(f) // pop a second time because it's a long, which occupies 2 slots
			val = val * (-1)
			push(f, val)
			push(f, val)
		case FNEG: //	0x76	(negate a float)
			val := pop(f).(float64)
			push(f, -val)

		case DNEG: // 0x77
			pop(f)
			val := pop(f).(float64)
			push(f, -val)
			push(f, -val)
		case ISHL: //	0x78 	(shift int left)
			shiftBy := pop(f).(int64)
			val1 := pop(f).(int64)
			var val2 int64
			if val1 < 0 { // if neg, shift as pos, then make neg
				val2 = (-val1) << (shiftBy & 0x1F) // only the bottom five bits are used
				push(f, -val2)
			} else {
				push(f, val1<<(shiftBy&0x1F))
			}

		case LSHL: // 	0x79	(shift value1 (long) left by value2 (int) bits)
			shiftBy := pop(f).(int64)
			ushiftBy := uint64(shiftBy) & 0x3f // must be unsigned in golang; 0-63 bits per JVM
			val1 := pop(f).(int64)
			pop(f)
			val3 := val1 << ushiftBy
			push(f, val3)
			push(f, val3)
		case ISHR: //  0x7A	(shift int value right)
			shiftBy := pop(f).(int64)
			val1 := pop(f).(int64)
			var val2 int64
			if val1 < 0 { // if neg, shift as pos, then make neg
				val2 = (-val1) >> (shiftBy & 0x1F) // only the bottom five bits are used
				push(f, -val2)
			} else {
				push(f, val1>>(shiftBy&0x1F))
			}
		case LSHR, // 	0x7B	(shift value1 (long) right by value2 (int) bits)
			LUSHR: // 	0x70
			shiftBy := pop(f).(int64)
			ushiftBy := uint64(shiftBy) & 0x3f // must be unsigned in golang; 0-63 bits per JVM
			val1 := pop(f).(int64)
			pop(f)
			val3 := val1 >> ushiftBy
			push(f, val3)
			push(f, val3)
		case IUSHR: // 0x7C (unsigned shift right of int)
			shiftBy := pop(f).(int64) // TODO: verify the result against JDK
			val1 := pop(f).(int64)
			if val1 < 0 {
				val1 = -val1
			}
			push(f, val1>>(shiftBy&0x1F)) // only the bottom five bits are used
		case IAND: //	0x7E	(logical and of two ints, push result)
			val1 := pop(f).(int64)
			val2 := pop(f).(int64)
			push(f, val1&val2)
		case LAND: //   0x7F    (logical and of two longs, push result)
			val1 := pop(f).(int64)
			pop(f)
			val2 := pop(f).(int64)
			pop(f)
			val3 := val1 & val2
			push(f, val3)
			push(f, val3)
		case IOR: // 0x 80 (logical OR of two ints, push result)
			val1 := pop(f).(int64)
			val2 := pop(f).(int64)
			push(f, val1|val2)
		case LOR: // 0x81  (logical OR of two longs, push result)
			val1 := pop(f).(int64)
			pop(f)
			val2 := pop(f).(int64)
			pop(f)
			val3 := val1 | val2
			push(f, val3)
			push(f, val3)
		case IXOR: // 	0x82	(logical XOR of two ints, push result)
			val1 := pop(f).(int64)
			val2 := pop(f).(int64)
			push(f, val1^val2)
		case LXOR: // 	0x83  	(logical XOR of two longs, push result)
			val1 := pop(f).(int64)
			pop(f)
			val2 := pop(f).(int64)
			pop(f)
			val3 := val1 ^ val2
			push(f, val3)
			push(f, val3)
		case IINC: // 	0x84    (increment local variable by a signed byte constant)
			localVarIndex := int64(f.Meth[f.PC+1])
			wbyte := f.Meth[f.PC+2]
			increment := byteToInt64(wbyte)
			orig := f.Locals[localVarIndex].(int64)
			f.Locals[localVarIndex] = orig + increment
			f.PC += 2
		case I2F: //	0x86 	( convert int to float)
			intVal := pop(f).(int64)
			push(f, float64(intVal))
		case I2L: // 	0x85     (convert int to long)
			// 	ints are already 64-bits, so this just pushes a second instance
			val := peek(f).(int64) // look without popping
			push(f, val)           // push the int a second time
		case I2D: // 	0x87	(convert int to double)
			intVal := pop(f).(int64)
			dval := float64(intVal)
			push(f, dval) // doubles use two slots, hence two pushes
			push(f, dval)
		case L2I: // 	0x88 	(convert long to int)
			longVal := pop(f).(int64)
			pop(f)
			intVal := longVal << 32 // remove high-end 4 bytes. this maintains the sign
			intVal >>= 32
			push(f, intVal)
		case L2F: // 	0x89 	(convert long to float)
			longVal := pop(f).(int64)
			pop(f)
			float32Val := float32(longVal) //
			float64Val := float64(float32Val)
			push(f, float64Val) // floats tke up only 1 slot in the JVM
		case L2D: // 	0x8A (convert long to double)
			longVal := pop(f).(int64)
			pop(f)
			dblVal := float64(longVal)
			push(f, dblVal)
			push(f, dblVal)
		case D2I: // 0xBE
			pop(f)
			fallthrough
		case F2I: // 0x8B
			floatVal := pop(f).(float64)
			push(f, int64(math.Trunc(floatVal)))
		case F2D: // 0x8D
			floatVal := pop(f).(float64)
			push(f, floatVal)
			push(f, floatVal)
		case D2L: // 	0x8F convert double to long
			pop(f)
			fallthrough
		case F2L: // 	0x8C convert float to long
			floatVal := pop(f).(float64)
			truncated := int64(math.Trunc(floatVal))
			push(f, truncated)
			push(f, truncated)

		case D2F: // 	0x90 Double to float
			floatVal := float32(pop(f).(float64))
			pop(f)
			push(f, float64(floatVal))
		case I2B: //	0x91 convert into to byte preserving sign
			intVal := pop(f).(int64)
			byteVal := intVal & 0xFF
			if !(intVal > 0 && byteVal > 0) &&
				!(intVal < 0 && byteVal < 0) {
				byteVal = -byteVal
			}
			push(f, byteVal)
		case I2C: //	0x92 convert to 16-bit char
			// determine what happens in Java if the int is negative
			intVal := pop(f).(int64)
			charVal := uint16(intVal) // Java chars are 16-bit unsigned value
			push(f, int64(charVal))
		case I2S: //	0x93 convert int to short
			intVal := pop(f).(int64)
			shortVal := int32(intVal)
			push(f, int64(shortVal))
		case LCMP: // 	0x94 (compare two longs, push int -1, 0, or 1, depending on result)
			value2 := pop(f).(int64)
			pop(f)
			value1 := pop(f).(int64)
			pop(f)
			if value1 == value2 {
				push(f, int64(0))
			} else if value1 > value2 {
				push(f, int64(1))
			} else {
				push(f, int64(-1))
			}
		case DCMPL, DCMPG: // 0x98, 0x97 - double comparison - they only differ in NaN treatment
			value2 := pop(f).(float64)
			pop(f)
			value1 := pop(f).(float64)
			pop(f)

			if math.IsNaN(value1) || math.IsNaN(value2) {
				if f.Meth[f.PC] == DCMPG {
					push(f, int64(1))
				} else {
					push(f, int64(-1))
				}
			} else if value1 > value2 {
				push(f, int64(1))
			} else if value1 < value2 {
				push(f, int64(-1))
			} else {
				push(f, int64(0))
			}
		case IFEQ: // 0x99 pop int, if it's == 0, go to the jump location
			// specified in the next two bytes
			value := pop(f).(int64)
			if value == 0 {
				jumpTo := (int16(f.Meth[f.PC+1]) * 256) + int16(f.Meth[f.PC+2])
				f.PC = f.PC + int(jumpTo) - 1
			} else {
				f.PC += 2
			}
		case IFNE: // 0x9A pop int, it it's !=0, go to the jump location
			// specified in the next two bytes
			value := pop(f).(int64)
			if value != 0 {
				jumpTo := (int16(f.Meth[f.PC+1]) * 256) + int16(f.Meth[f.PC+2])
				f.PC = f.PC + int(jumpTo) - 1
			} else {
				f.PC += 2
			}
		case IFLT: // 0x9B pop int, if it's < 0, go to the jump location
			// specified in the next two bytes
			value := pop(f).(int64)
			if value < 0 {
				jumpTo := (int16(f.Meth[f.PC+1]) * 256) + int16(f.Meth[f.PC+2])
				f.PC = f.PC + int(jumpTo) - 1
			} else {
				f.PC += 2
			}
		case IFGE: // 0x9C pop int, if it's >= 0, go to the jump location
			// specified in the next two bytes
			value := pop(f).(int64)
			if value >= 0 {
				jumpTo := (int16(f.Meth[f.PC+1]) * 256) + int16(f.Meth[f.PC+2])
				f.PC = f.PC + int(jumpTo) - 1
			} else {
				f.PC += 2
			}
		case IFGT: // 0x9D pop int, if it's > 0, go to the jump location
			// specified in the next two bytes
			value := pop(f).(int64)
			if value > 0 {
				jumpTo := (int16(f.Meth[f.PC+1]) * 256) + int16(f.Meth[f.PC+2])
				f.PC = f.PC + int(jumpTo) - 1
			} else {
				f.PC += 2
			}
		case IFLE: // 0x9E pop int, if it's <= 0, go to the jump location
			// specified in the next two bytes
			value := pop(f).(int64)
			if value <= 0 {
				jumpTo := (int16(f.Meth[f.PC+1]) * 256) + int16(f.Meth[f.PC+2])
				f.PC = f.PC + int(jumpTo) - 1
			} else {
				f.PC += 2
			}
		case IF_ICMPEQ: //  0x9F 	(jump if top two ints are equal)
			val2 := pop(f).(int64)
			val1 := pop(f).(int64)
			if int32(val1) == int32(val2) { // if comp succeeds, next 2 bytes hold instruction index
				jumpTo := (int16(f.Meth[f.PC+1]) * 256) + int16(f.Meth[f.PC+2])
				f.PC = f.PC + int(jumpTo) - 1 // -1 b/c on the next iteration, pc is bumped by 1
			} else {
				f.PC += 2
			}
		case IF_ICMPNE: //  0xA0    (jump if top two ints are not equal)
			val2 := pop(f).(int64)
			val1 := pop(f).(int64)
			if int32(val1) != int32(val2) { // if comp succeeds, next 2 bytes hold instruction index
				jumpTo := (int16(f.Meth[f.PC+1]) * 256) + int16(f.Meth[f.PC+2])
				f.PC = f.PC + int(jumpTo) - 1 // -1 b/c on the next iteration, pc is bumped by 1
			} else {
				f.PC += 2
			}
		case IF_ICMPLT: //  0xA1    (jump if popped val1 < popped val2)
			val2 := pop(f).(int64)
			val1 := pop(f).(int64)
			val1a := val1
			val2a := val2
			if val1a < val2a { // if comp succeeds, next 2 bytes hold instruction index
				jumpTo := (int16(f.Meth[f.PC+1]) * 256) + int16(f.Meth[f.PC+2])
				f.PC = f.PC + int(jumpTo) - 1 // -1 b/c on the next iteration, pc is bumped by 1
			} else {
				f.PC += 2
			}
		case IF_ICMPGE: //  0xA2    (jump if popped val1 >= popped val2)
			val2 := pop(f).(int64)
			val1 := pop(f).(int64)
			if val1 >= val2 { // if comp succeeds, next 2 bytes hold instruction index
				jumpTo := (int16(f.Meth[f.PC+1]) * 256) + int16(f.Meth[f.PC+2])
				f.PC = f.PC + int(jumpTo) - 1 // -1 b/c on the next iteration, pc is bumped by 1
			} else {
				f.PC += 2
			}
		case IF_ICMPGT: //  0xA3    (jump if popped val1 > popped val2)
			val2 := pop(f).(int64)
			val1 := pop(f).(int64)
			if int32(val1) > int32(val2) { // if comp succeeds, next 2 bytes hold instruction index
				jumpTo := (int16(f.Meth[f.PC+1]) * 256) + int16(f.Meth[f.PC+2])
				f.PC = f.PC + int(jumpTo) - 1 // -1 b/c on the next iteration, pc is bumped by 1
			} else {
				f.PC += 2
			}
		case IF_ICMPLE: //	0xA4	(jump if popped val1 <= popped val2)
			val2 := pop(f).(int64)
			val1 := pop(f).(int64)
			if val1 <= val2 { // if comp succeeds, next 2 bytes hold instruction index
				jumpTo := (int16(f.Meth[f.PC+1]) * 256) + int16(f.Meth[f.PC+2])
				f.PC = f.PC + int(jumpTo) - 1 // -1 b/c on the next iteration, pc is bumped by 1
			} else {
				f.PC += 2
			}
		case IF_ACMPEQ: // 0xA5		(jump if two addresses are equal)
			val2 := pop(f).(int64)
			val1 := pop(f).(int64)
			if val1 == val2 { // if comp succeeds, next 2 bytes hold instruction index
				jumpTo := (int16(f.Meth[f.PC+1]) * 256) + int16(f.Meth[f.PC+2])
				f.PC = f.PC + int(jumpTo) - 1 // -1 b/c on the next iteration, pc is bumped by 1
			} else {
				f.PC += 2
			}
		case IF_ACMPNE: // 0xA6		(jump if two addresses are note equal)
			val2 := pop(f).(int64)
			val1 := pop(f).(int64)
			if val1 != val2 { // if comp succeeds, next 2 bytes hold instruction index
				jumpTo := (int16(f.Meth[f.PC+1]) * 256) + int16(f.Meth[f.PC+2])
				f.PC = f.PC + int(jumpTo) - 1 // -1 b/c on the next iteration, pc is bumped by 1
			} else {
				f.PC += 2
			}
		case GOTO: // 0xA7     (goto an instruction)
			jumpTo := (int16(f.Meth[f.PC+1]) * 256) + int16(f.Meth[f.PC+2])
			f.PC = f.PC + int(jumpTo) - 1 // -1 because this loop will increment f.PC by 1
		case IRETURN: // 0xAC (return an int and exit current frame)
			valToReturn := pop(f)
			f = fs.Front().Next().Value.(*frames.Frame)
			push(f, valToReturn) // TODO: check what happens when main() ends on IRETURN
			return nil
		case LRETURN: // 0xAD (return a long and exit current frame)
			valToReturn := pop(f).(int64)
			f = fs.Front().Next().Value.(*frames.Frame)
			push(f, valToReturn) // pushed twice b/c a long uses two slots
			push(f, valToReturn)
			return nil
		case FRETURN: // 0xAE
			valToReturn := pop(f).(float64)
			f = fs.Front().Next().Value.(*frames.Frame)
			push(f, valToReturn)
			return nil
		case DRETURN: // 0xAF (return a double and exit current frame)
			valToReturn := pop(f).(float64)
			f = fs.Front().Next().Value.(*frames.Frame)
			push(f, valToReturn) // pushed twice b/c a float uses two slots
			push(f, valToReturn)
			return nil
		case ARETURN: // 0xB0	(return a reference)
			valToReturn := pop(f)
			f = fs.Front().Next().Value.(*frames.Frame)
			push(f, valToReturn)
			return nil
		case RETURN: // 0xB1    (return from void function)
			f.TOS = -1 // empty the stack
			return nil
		case GETSTATIC: // 0xB2		(get static field)
			// TODO: getstatic will instantiate a static class if it's not already instantiated
			// that logic has not yet been implemented and the code here is simply a reasonable
			// placeholder, which consists of creating a struct that holds most of the needed info
			// puts it into a slice of such static fields and pushes the index of this item in the slice
			// onto the stack of the frame.
			CPslot := (int(f.Meth[f.PC+1]) * 256) + int(f.Meth[f.PC+2]) // next 2 bytes point to CP entry
			f.PC += 2
			CPentry := f.CP.CpIndex[CPslot]
			if CPentry.Type != classloader.FieldRef { // the pointed-to CP entry must be a field reference
				return fmt.Errorf("GETSTATIC: Expected a field ref, but got %d in"+
					"location %d in method %s of class %s\n",
					CPentry.Type, f.PC, f.MethName, f.ClName)
			}

			// get the field entry
			field := f.CP.FieldRefs[CPentry.Slot]

			// get the class entry from the field entry for this field. It's the class name.
			classRef := field.ClassIndex
			classNameIndex := f.CP.ClassRefs[f.CP.CpIndex[classRef].Slot]
			classNameEntry := f.CP.CpIndex[classNameIndex]
			className := f.CP.Utf8Refs[classNameEntry.Slot]

			// process the name and type entry for this field
			nAndTindex := field.NameAndType
			nAndTentry := f.CP.CpIndex[nAndTindex]
			nAndTslot := nAndTentry.Slot
			nAndT := f.CP.NameAndTypes[nAndTslot]
			fieldNameIndex := nAndT.NameIndex
			fieldName := classloader.FetchUTF8stringFromCPEntryNumber(f.CP, fieldNameIndex)
			fieldName = className + "." + fieldName

			// was this static field previously loaded? Is so, get its location and move on.
			prevLoaded, ok := classloader.Statics[fieldName]
			if ok { // if preloaded, then push the index into the array of constant fields
				push(f, prevLoaded)
				break
			}

			fieldTypeIndex := nAndT.DescIndex
			fieldType := classloader.FetchUTF8stringFromCPEntryNumber(f.CP, fieldTypeIndex)
			// println("full field name: " + fieldName + ", type: " + fieldType)
			newStatic := classloader.Static{
				Class:     'L',
				Type:      fieldType,
				ValueRef:  "",
				ValueInt:  0,
				ValueFP:   0,
				ValueStr:  "",
				ValueFunc: nil,
				CP:        f.CP,
			}
			classloader.StaticsArray = append(classloader.StaticsArray, newStatic)
			classloader.Statics[fieldName] = int64(len(classloader.StaticsArray) - 1)

			// push the pointer to the stack of the frame
			push(f, int64(len(classloader.StaticsArray)-1))

		case GETFIELD: // 0xB4 get field in pointed-to-object
			CPslot := (int(f.Meth[f.PC+1]) * 256) + int(f.Meth[f.PC+2]) // next 2 bytes point to CP entry
			f.PC += 2
			fieldEntry := f.CP.CpIndex[CPslot]
			if fieldEntry.Type != classloader.FieldRef { // the pointed-to CP entry must be a method reference
				return fmt.Errorf("GETFIELD: Expected a field ref, but got %d in"+
					"location %d in method %s of class %s\n",
					fieldEntry.Type, f.PC, f.MethName, f.ClName)
			}

			ref := pop(f).(*object.Object)
			obj := *ref
			// fieldType := obj.Fields[fieldEntry.Slot].Ftype
			fieldValue := obj.Fields[fieldEntry.Slot].Fvalue
			push(f, fieldValue)

		case PUTFIELD: // 0xB5 place value into an object's field
			CPslot := (int(f.Meth[f.PC+1]) * 256) + int(f.Meth[f.PC+2]) // next 2 bytes point to CP entry
			f.PC += 2
			fieldEntry := f.CP.CpIndex[CPslot]
			if fieldEntry.Type != classloader.FieldRef { // the pointed-to CP entry must be a method reference
				return fmt.Errorf("PUTFIELD: Expected a field ref, but got %d in"+
					"location %d in method %s of class %s\n",
					fieldEntry.Type, f.PC, f.MethName, f.ClName)
			}

			value := pop(f)
			ref := pop(f).(*object.Object)
			obj := *ref

			// the fields in the object are numbered in the same
			// order they are declared in the constant pool. So,
			// to get to the right field, we only need to know
			// the slot number in CP.Fields. It will be the same
			// index into the object's fields.
			obj.Fields[fieldEntry.Slot].Fvalue = value

		case INVOKEVIRTUAL: // 	0xB6 invokevirtual (create new frame, invoke function)
			var err error
			CPslot := (int(f.Meth[f.PC+1]) * 256) + int(f.Meth[f.PC+2]) // next 2 bytes point to CP entry
			f.PC += 2

			CPentry := f.CP.CpIndex[CPslot]
			if CPentry.Type != classloader.MethodRef { // the pointed-to CP entry must be a method reference
				return fmt.Errorf("INVOKEVIRTUAL: Expected a method ref, but got %d in"+
					"location %d in method %s of class %s\n",
					CPentry.Type, f.PC, f.MethName, f.ClName)
			}

			// get the methodRef entry
			method := f.CP.MethodRefs[CPentry.Slot]

			// get the class entry from this method
			classRef := method.ClassIndex
			classNameIndex := f.CP.ClassRefs[f.CP.CpIndex[classRef].Slot]
			classNameEntry := f.CP.CpIndex[classNameIndex]
			className := f.CP.Utf8Refs[classNameEntry.Slot]

			// get the method name for this method
			nAndTindex := method.NameAndType
			nAndTentry := f.CP.CpIndex[nAndTindex]
			nAndTslot := nAndTentry.Slot
			nAndT := f.CP.NameAndTypes[nAndTslot]
			methodNameIndex := nAndT.NameIndex
			methodName := classloader.FetchUTF8stringFromCPEntryNumber(f.CP, methodNameIndex)
			// methodName = className + "." + methodName

			// get the signature for this method
			methodSigIndex := nAndT.DescIndex
			methodType := classloader.FetchUTF8stringFromCPEntryNumber(f.CP, methodSigIndex)

			mtEntry := classloader.MTable[className+"."+methodName+methodType]
			if mtEntry.Meth == nil { // if the method is not in the method table, find it
				mtEntry, err = classloader.FetchMethodAndCP(className, methodName, methodType)
				if err != nil || mtEntry.Meth == nil {
					// TODO: search the classpath and retry
					return errors.New("INVOKEVIRTUAL: Class not found: " + className + "." + methodName)
				}
			}

			if mtEntry.MType == 'G' { // so we have a golang function
				_, err := runGmethod(mtEntry, fs, className, methodName, methodType)
				if err != nil {
					shutdown.Exit(shutdown.APP_EXCEPTION) // any exceptions message will already have been displayed to the user
				}
				break
			}

			if mtEntry.MType == 'J' { // it's a Java function (that is, non-native)
				m := mtEntry.Meth.(classloader.JmEntry)
				fram, err := createAndInitNewFrame(
					className, methodName, methodType, &m, true, f)
				if err != nil {
					return errors.New("INVOKEVIRTUAL: Error creating frame in: " +
						className + "." + methodName)
				}

				fs.PushFront(fram)                   // push the new frame
				f = fs.Front().Value.(*frames.Frame) // point f to the new head
				err = runFrame(fs)                   // 2nd on stack from new crash site
				if err != nil {
					return err
				}

				// if the method is main(), then when we get here the
				// frame stack will be empty to exit from here, otherwise
				// there's still a frame on the stack, pop it off and continue.
				if fs.Len() == 0 {
					return nil
				}
				fs.Remove(fs.Front()) // pop the frame off

				// the previous frame pop might have been main()
				// if so, then we can't reset f to a non-existent frame
				// so we test for this before resetting f.
				if fs.Len() != 0 {
					f = fs.Front().Value.(*frames.Frame)
				} else {
					return nil
				}
			}
		case INVOKESPECIAL: //	0xB7 invokespecial (invoke constructors, private methods, etc.)
			CPslot := (int(f.Meth[f.PC+1]) * 256) + int(f.Meth[f.PC+2]) // next 2 bytes point to CP entry
			f.PC += 2
			className, methName, methSig := getMethInfoFromCPmethref(f.CP, CPslot)
			// MethodName := getClassNameFromCPclassref(f.CP, MethodClassIdx)

			mtEntry, err := classloader.FetchMethodAndCP(className, methName, methSig)
			if err != nil {
				return errors.New("INVOKESPECIAL: Class not found: " + className + "." + methName)
			}

			if mtEntry.MType == 'G' { // it's a golang method
				f, err = runGmethod(mtEntry, fs, className, className+"."+methName, methSig)
				if err != nil {
					shutdown.Exit(shutdown.APP_EXCEPTION) // any exceptions message will already have been displayed to the user
				}
			} else if mtEntry.MType == 'J' {
				// TODO: handle arguments to method, if any
				m := mtEntry.Meth.(classloader.JmEntry)
				fram, err := createAndInitNewFrame(
					className, methName, methSig, &m, true, f)
				if err != nil {
					return errors.New("INVOKESPECIAL: Error creating frame in: " +
						className + "." + methName)
				}

				fs.PushFront(fram)                   // push the new frame
				f = fs.Front().Value.(*frames.Frame) // point f to the new head
				err = runFrame(fs)                   // 2nd on stack from new crash site
				if err != nil {
					return err
				}

				// // if the method is main(), then when we get here the
				// // frame stack will be empty to exit from here, otherwise
				// // there's still a frame on the stack, pop it off and continue.
				// if fs.Len() == 0 {
				// 	return nil
				// }
				fs.Remove(fs.Front()) // pop the frame off

				// the previous frame pop might have been main()
				// if so, then we can't reset f to a non-existent frame
				// so we test for this before resetting f.
				if fs.Len() != 0 {
					f = fs.Front().Value.(*frames.Frame)
				} else {
					return nil
				}
			}
		case INVOKESTATIC: // 	0xB8 invokestatic (create new frame, invoke static function)
			CPslot := (int(f.Meth[f.PC+1]) * 256) + int(f.Meth[f.PC+2]) // next 2 bytes point to CP entry
			f.PC += 2
			CPentry := f.CP.CpIndex[CPslot]
			// get the methodRef entry
			method := f.CP.MethodRefs[CPentry.Slot]

			// get the class entry from this method
			classRef := method.ClassIndex
			classNameIndex := f.CP.ClassRefs[f.CP.CpIndex[classRef].Slot]
			classNameEntry := f.CP.CpIndex[classNameIndex]
			className := f.CP.Utf8Refs[classNameEntry.Slot]

			// get the method name for this method
			nAndTindex := method.NameAndType
			nAndTentry := f.CP.CpIndex[nAndTindex]
			nAndTslot := nAndTentry.Slot
			nAndT := f.CP.NameAndTypes[nAndTslot]
			methodNameIndex := nAndT.NameIndex
			methodName := classloader.FetchUTF8stringFromCPEntryNumber(f.CP, methodNameIndex)
			// println("Method name for invokestatic: " + className + "." + methodName)

			// get the signature for this method
			methodSigIndex := nAndT.DescIndex
			methodType := classloader.FetchUTF8stringFromCPEntryNumber(f.CP, methodSigIndex)
			// println("Method signature for invokestatic: " + methodName + methodType)

			// m, cpp, err := fetchMethodAndCP(className, methodName, methodType)
			mtEntry, err := classloader.FetchMethodAndCP(className, methodName, methodType)
			if err != nil {
				return errors.New("INVOKESTATIC: Class not found: " + className + methodName)
			}

			if mtEntry.MType == 'G' {
				f, err = runGmethod(mtEntry, fs, className, methodName, methodType)

				if err != nil {
					shutdown.Exit(shutdown.APP_EXCEPTION) // any exceptions message will already have been displayed to the user
				}
			} else if mtEntry.MType == 'J' {
				m := mtEntry.Meth.(classloader.JmEntry)
				fram, err := createAndInitNewFrame(
					className, methodName, methodType, &m, false, f)
				if err != nil {
					return errors.New("INVOKESTATIC: Error creating frame in: " +
						className + "." + methodName)
				}

				fs.PushFront(fram)                   // push the new frame
				f = fs.Front().Value.(*frames.Frame) // point f to the new head
				err = runFrame(fs)                   // 2nd on stack from new crash site
				if err != nil {
					return err
				}

				// if the static method is main(), when we get here the
				// frame stack will be empty to exit from here, otherwise
				// there's still a frame on the stack, pop it off and continue.
				if fs.Len() == 0 {
					return nil
				}
				fs.Remove(fs.Front()) // pop the frame off

				// the previous frame pop might have been main()
				// if so, then we can't reset f to a non-existent frame
				// so we test for this before resetting f.
				if fs.Len() != 0 {
					f = fs.Front().Value.(*frames.Frame)
				} else {
					return nil
				}
			}
		case NEW: // 0xBB 	new: create and instantiate a new object
			CPslot := (int(f.Meth[f.PC+1]) * 256) + int(f.Meth[f.PC+2]) // next 2 bytes point to CP entry
			f.PC += 2
			CPentry := f.CP.CpIndex[CPslot]
			if CPentry.Type != classloader.ClassRef && CPentry.Type != classloader.Interface {
				msg := fmt.Sprintf("NEW: Invalid type for new object")
				_ = log.Log(msg, log.SEVERE)
			}

			// the classref points to a UTF8 record with the name of the class to instantiate
			var className string
			if CPentry.Type == classloader.ClassRef {
				utf8Index := f.CP.ClassRefs[CPentry.Slot]
				className = classloader.FetchUTF8stringFromCPEntryNumber(f.CP, utf8Index)
			}

			ref, err := instantiateClass(className)
			if err != nil {
				// error message(s) already shown to user
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}
			push(f, ref)

		case NEWARRAY: // 0xBC create a new array of primitives
			size := pop(f).(int64)
			if size < 0 {
				exceptions.Throw(
					exceptions.NegativeArraySizeException,
					"NEWARRAY: Invalid size for array")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}

			arrayType := int(f.Meth[f.PC+1])
			f.PC += 1

			g := globals.GetGlobalRef()

			actualType := object.JdkArrayTypeToJacobinType(arrayType)
			if actualType == object.ERROR {
				_ = log.Log("NEWARRAY: Invalid array type specified", log.SEVERE)
				return errors.New("NEWARRAY: error instantiating array")
			}

			arrayPtr := object.Make1DimArray(uint8(actualType), size)
			g.ArrayAddressList.PushFront(arrayPtr)
			push(f, arrayPtr)

		case ANEWARRAY: // 0xBD create array of references
			size := pop(f).(int64)
			if size < 0 {
				exceptions.Throw(
					exceptions.NegativeArraySizeException,
					"ANEWARRAY: Invalid size for array")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}

			arrayPtr := object.Make1DimArray(object.REF, size)
			g := globals.GetGlobalRef()
			g.ArrayAddressList.PushFront(arrayPtr)
			push(f, arrayPtr)

			// The bytecode is followed by a two-byte index into the CP
			// which indicates what type the reference points to. We
			// don't presently check the type, so we skip over these
			// two bytes.
			f.PC += 2

		case ARRAYLENGTH: // OxBE get size of array
			// expects a pointer to an array
			ref := pop(f).(*object.Object)
			if ref == nil {
				exceptions.Throw(exceptions.NullPointerException,
					"ARRAYLENGTH: Invalid (null) reference to an array")
				shutdown.Exit(shutdown.APP_EXCEPTION)
			}

			var size int64
			arrayType := ref.Fields[0].Ftype
			switch arrayType {
			case "[B":
				arrayPtr := ref.Fields[0].Fvalue.(*[]javaTypes.JavaByte)
				size = int64(len(*arrayPtr))
			case "[L":
				arrayPtr := ref.Fields[0].Fvalue.(*[]*object.Object)
				size = int64(len(*arrayPtr))
			case "[F":
				arrayPtr := ref.Fields[0].Fvalue.(*[]float64)
				size = int64(len(*arrayPtr))
			default:
				arrayPtr := ref.Fields[0].Fvalue.(*[]int64)
				size = int64(len(*arrayPtr))
			}

			push(f, size)

		case MULTIANEWARRAY: // 0xC5 create multi-dimensional array
			var arrayDesc string
			var arrayType uint8

			// The first two chars after the bytecode point to a
			// classref entry in the CP. In turn, it points to a
			// string describing the array. Of the form [[L or
			// similar, in which one [ is present for every dimension
			// followed by a single letter describing the type of
			// entry in the final dimension of the array. The letters
			// are the usual ones used in the JVM for primitives, etc.
			// as in: https://docs.oracle.com/javase/specs/jvms/se17/html/jvms-4.html#jvms-4.3.2-200
			CPslot := (int(f.Meth[f.PC+1]) * 256) + int(f.Meth[f.PC+2]) // next 2 bytes point to CP entry
			f.PC += 2
			CPentry := f.CP.CpIndex[CPslot]
			if CPentry.Type != classloader.ClassRef {
				return errors.New("MULTIANEWARRAY: multi-dimensional array presently supports classes only")
			} else {
				utf8Index := f.CP.ClassRefs[CPentry.Slot]
				arrayDesc = classloader.FetchUTF8stringFromCPEntryNumber(f.CP, utf8Index)
			}

			var rawArrayType uint8
			for i := 0; i < len(arrayDesc); i++ {
				if arrayDesc[i] != '[' {
					rawArrayType = arrayDesc[i]
				}
			}

			switch rawArrayType {
			case 'B', 'Z':
				arrayType = object.BYTE
			case 'F', 'D':
				arrayType = object.FLOAT
			case 'L':
				arrayType = object.REF
			default:
				arrayType = object.INT
			}

			// get the number of dimensions, then pop off the operand
			// stack an int for every dimension, giving the size of that
			// dimension and put them into a slice that starts with
			// the highest dimension first. So a two-dimensional array
			// such as x[4][3], would have entries of 4 and 3 respectively
			// in the dimsizes slice.
			dimensionCount := int(f.Meth[f.PC+1])
			f.PC += 1

			if dimensionCount > 3 { // TODO: explore arrays of > 5-256 dimensions
				_ = log.Log("MULTIANEWARRAY: Jacobin supports arrays only up to three dimensions",
					log.SEVERE)
				return errors.New(
					"MULTIANEWARRAY: Jacobin supports arrays only up to three dimensions")
			}

			dimSizes := make([]int64, dimensionCount)

			// the values on the operand stack give the last dimension
			// first when popped off the stack, so, they're stored here
			// in reverse order, so that dimSizes[0] will hold the first
			// dimenion.
			for i := dimensionCount - 1; i >= 0; i-- {
				dimSizes[i] = pop(f).(int64)
			}

			// A dimension of zero ends the dimensions, so we check
			// and cut off the dimensions below and includingthe 0-sized
			// one. Because this is almost certainly an error, we also
			// issue a warning.
			for i := range dimSizes {
				if dimSizes[i] == 0 {
					dimSizes = dimSizes[i+1:] // lop off the prev dims
					_ = log.Log("MULTIANEWARRAY: Multidimensional array with one dimension of size 0 encountered.",
						log.WARNING)
					break
				}
			}

			// Because of the possibility of a zero-sized dimension
			// affecting the valid number of dimensions, dimensionCount
			// can no longer be considered reliable. Use len(dimSizes).
			if len(dimSizes) == 3 {
				multiArr := object.Make1DimArray(object.REF, dimSizes[0])
				actualArray := multiArr.Fields[0].Fvalue.([]*object.Object)
				for i := 0; i < len(actualArray); i++ {
					actualArray[i], _ = object.Make2DimArray(dimSizes[1],
						dimSizes[2], arrayType)
				}
				push(f, multiArr)
				f.PC += 1
				continue
			} else if len(dimSizes) == 2 { // 2-dim array is a special, trivial case
				multiArr, _ := object.Make2DimArray(dimSizes[0], dimSizes[1], arrayType)
				push(f, multiArr)
				f.PC += 1
				continue
				// It's possible due to a zero-length dimension, that we
				// need to create a single-dimension array.
			} else if len(dimSizes) == 1 {
				oneDimArr := object.Make1DimArray(arrayType, dimSizes[0])
				push(f, oneDimArr)
				f.PC += 1
				continue
			}

		case IFNULL: // 0xC6 jump if TOS holds a null address
			// null = 0, so we duplicate logic of IFEQ instruction
			value := pop(f).(int64)
			if value == 0 {
				jumpTo := (int16(f.Meth[f.PC+1]) * 256) + int16(f.Meth[f.PC+2])
				f.PC = f.PC + int(jumpTo) - 1
			} else {
				f.PC += 2
			}
		case IFNONNULL: // 0xC7 jump if TOS does not hold a null address
			// null = 0, so we duplicate logic of IFNE instruction
			value := pop(f).(int64)
			if value != 0 {
				jumpTo := (int16(f.Meth[f.PC+1]) * 256) + int16(f.Meth[f.PC+2])
				f.PC = f.PC + int(jumpTo) - 1
			} else {
				f.PC += 2
			}
		default:
			missingOpCode := fmt.Sprintf("%d (0x%X)", f.Meth[f.PC], f.Meth[f.PC])

			if int(f.Meth[f.PC]) < len(BytecodeNames) && int(f.Meth[f.PC]) > 0 {
				missingOpCode += fmt.Sprintf("(%s)", BytecodeNames[f.Meth[f.PC]])
			}

			msg := fmt.Sprintf("Invalid bytecode found: %s at location %d in method %s() of class %s\n",
				missingOpCode, f.PC, f.MethName, f.ClName)
			_ = log.Log(msg, log.SEVERE)
			return errors.New("invalid bytecode encountered")
		}
		f.PC += 1
	}
	return nil
}

// pop from the operand stack. TODO: need to put in checks for invalid pops
func pop(f *frames.Frame) interface{} {
	value := f.OpStack[f.TOS]
	f.TOS -= 1
	if MainThread.Trace {
		traceInfo := fmt.Sprintf("\tpop f.TOS=%d", f.TOS)
		_ = log.Log(traceInfo, log.TRACE_INST)
	}
	return value
}

// returns the value at the top of the stack without popping it off.
func peek(f *frames.Frame) interface{} {
	if MainThread.Trace {
		traceInfo := fmt.Sprintf("\tpeek f.TOS=%d", f.TOS)
		_ = log.Log(traceInfo, log.TRACE_INST)
	}
	return f.OpStack[f.TOS]
}

// push onto the operand stack
func push(f *frames.Frame, x interface{}) {
	f.TOS += 1
	f.OpStack[f.TOS] = x
	if MainThread.Trace {
		traceInfo := fmt.Sprintf("\tpush f.TOS=%d", f.TOS)
		_ = log.Log(traceInfo, log.TRACE_INST)
	}
}

func add[N frames.Number](num1, num2 N) N {
	return num1 + num2
}

// multiply two numbers
func multiply[N frames.Number](num1, num2 N) N {
	return num1 * num2
}

func subtract[N frames.Number](num1, num2 N) N {
	return num1 - num2
}

// converts an interface{} value to int8. Used for BASTORE
func convertInterfaceToInt8(val interface{}) int8 {
	switch t := val.(type) {
	case int64:
		return int8(t)
	case int:
		return int8(t)
	case int8:
		return t
	}
	return 0
}

// converts an interface{} value into uint64
func convertInterfaceToUint64(val interface{}) uint64 {
	// in theory, the only types passed to this function are those
	// found on the operand stack: ints, floats, pointers
	switch t := val.(type) {
	case int64:
		return uint64(t)
	case float64:
		return uint64(math.Round(t))
	case unsafe.Pointer:
		intVal := uintptr(t)
		return uint64(intVal)
	}
	return 0
}

// ** Presently unused. May be deleted if not found useful
// converts an interface{} value into unsafe.Pointer
// func convertInterfaceToPointer(val interface{}) unsafe.Pointer {
// 	var ptr unsafe.Pointer
// 	switch t := val.(type) {
// 	case int64, int:
// 		uip := uintptr(&t)
// 		ptr = unsafe.Pointer(uip)
// 	case byte:
// 		uip := uintptr(t)
// 		ptr = unsafe.Pointer(uip)
// 	case float64:
// 		ptr = nil
// 	case unsafe.Pointer:
// 		ptr = t
// 	}
// 	return ptr
// }

// create a new frame and load up the local variables with the passed
// arguments, set up the stack, and all the remaining items to begin execution
// Note: the includeOjectRef parameter is a boolean. When true, it indicates
// that in addition to the method parameter, an object reference is also on
// the stack and needs to be popped off the caller's opStack and passed in.
// (This would be the case for invokevirtual, among others.) When false, no
// object pointer is needed (for invokestatic, among others).
func createAndInitNewFrame(
	className string, methodName string, methodType string,
	m *classloader.JmEntry,
	includeOjectRef bool,
	currFrame *frames.Frame) (*frames.Frame, error) {
	f := currFrame

	fram := frames.CreateFrame(m.MaxStack)
	fram.ClName = className
	fram.MethName = methodName
	fram.CP = m.Cp                     // add its pointer to the class CP
	for i := 0; i < len(m.Code); i++ { // copy the method's bytecodes over
		fram.Meth = append(fram.Meth, m.Code[i])
	}

	// allocate the local variables
	for k := 0; k < m.MaxLocals; k++ {
		fram.Locals = append(fram.Locals, int64(0))
	}

	// pop the parameters off the present stack and put them in
	// the new frame's locals. This is done in reverse order so
	// that the parameters are pushed in the right order to be
	// popped off by the receiving function
	var argList []interface{}
	paramsToPass :=
		util.ParseIncomingParamsFromMethTypeString(methodType)

	// primitives use a single byte/letter, but arrays can be many bytes:
	// a minimum of two (e.g., [I for array of ints). If the array
	// is multidimensional, the bytes will be [[I with one instance
	// of [ for every dimension. In the case of multidimensional
	// arrays, the arrays are always pushed as arrays of references,
	// and we simply mark off the number of [. For single-dimensional
	// arrays, we pass the kind of pointer that applies and mark off
	// a single instance of [
	for j := len(paramsToPass) - 1; j > -1; j-- {
		param := paramsToPass[j]
		primitive := param[0]

		arrayDimensions := 0
		if primitive == '[' {
			i := 0
			for i = 0; i < len(param); i++ {
				if param[i] == '[' {
					arrayDimensions += 1
				} else {
					break
				}
			}
			// param[i] now holds the primitive of the array
			primitive = param[i]
		}

		if arrayDimensions > 1 { // a multidimensional array
			// if the array is multidimensional, then we are
			// passing in an pointer to an array of references
			// to objects (lower arrays) regardless of the
			// lowest level of primitive in the array
			arg := pop(f).(*object.Object)
			argList = append(argList, arg)
			continue
		}

		if arrayDimensions == 1 { // a single-dimension array
			arg := pop(f).(*object.Object)
			argList = append(argList, arg)
			continue
		}

		switch primitive { // it's not an array
		case 'D': // double
			arg := pop(f).(float64)
			argList = append(argList, arg)
			argList = append(argList, arg)
			pop(f)
		case 'F': // float
			arg := pop(f).(float64)
			argList = append(argList, arg)
		case 'B', 'C', 'I', 'S': // byte, char, integer, short
			arg := pop(f).(int64)
			argList = append(argList, arg)
		case 'J': // long
			arg := pop(f).(int64)
			argList = append(argList, arg)
			argList = append(argList, arg)
			pop(f)
		case 'L': // pointer/reference
			arg := pop(f).(*object.Object)
			argList = append(argList, arg)
		default:
			arg := pop(f)
			argList = append(argList, arg)
		}
	}

	// if objectRef != nil, insert it in the local[0]
	// this is used in invokevirtual, invokespecial, and
	// invokeinterface.
	destLocal := 0
	if includeOjectRef {
		fram.Locals[0] = pop(f)
		destLocal = 1
	}

	for j := len(argList) - 1; j >= 0; j-- {
		fram.Locals[destLocal] = argList[j]
		destLocal += 1
	}
	fram.TOS = -1

	return fram, nil
}
