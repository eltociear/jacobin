/*
 * Jacobin VM - A Java virtual machine
 * Copyright (c) 2022-23 by the Jacobin authors. All rights reserved.
 * Licensed under Mozilla Public License 2.0 (MPL 2.0)
 */

package jvm

import (
	"errors"
	"fmt"
	"jacobin/classloader"
	// "jacobin/frames"
	// "jacobin/globals"
	"jacobin/log"
	"jacobin/object"
	"jacobin/shutdown"
	// "jacobin/thread"
	"jacobin/types"
	// "strconv"
	"strings"
	"unsafe"
)

// instantiating an object is a two-part process (except for arrays, which are handled
// by special bytecodes):
//  1. the class needs to be loaded, so that its details and its methods are knowable
//  2. the class fields (if static) and instance fields (if non-static) are allocated.
//     Details for this second step appear in the loop that drives createField().
func instantiateClass(classname string) (*object.Object, error) {

	if !strings.HasPrefix(classname, "[") { // do this only for classes, not arrays
		err := loadThisClass(classname)
		if err != nil { // error message will have been displayed
			return nil, err
		}
	}

	// At this point, classname is ready
	k := classloader.MethAreaFetch(classname)
	obj := object.Object{
		Klass: &classname,
	}

	if k == nil {
		errMsg := "Class is nil after loading, class: " + classname
		_ = log.Log(errMsg, log.SEVERE)
		return nil, errors.New(errMsg)
	}

	if k.Data == nil {
		errMsg := "class.Data is nil, class: " + classname
		_ = log.Log(errMsg, log.SEVERE)
		return nil, errors.New(errMsg)
	}

	// go up the chain of superclasses until we hit java/lang/Object
	superclasses := []string{}
	superclass := k.Data.Superclass
	for {
		if superclass == "java/lang/Object" {
			break
		}

		err := loadThisClass(superclass) // load the superclass
		if err != nil {                  // error message will have been displayed
			return nil, err
		} else {
			superclasses = append(superclasses, superclass)
		}

		loadedSuperclass := classloader.MethAreaFetch(superclass)
		// now loop to see whether this superclass has a superclass
		superclass = loadedSuperclass.Data.Superclass
	}

	// the object's mark field contains the lower 32-bits of the object's
	// address, which serves as the hash code for the object
	uintp := uintptr(unsafe.Pointer(&obj))
	obj.Mark.Hash = uint32(uintp)

	// handle the fields. If the object has no superclass other than Object,
	// the fields are in an array in the order they're declared in the CP.
	// If the object has a non-Object superclass, then the superclasses' fields
	// and the present object's field are stored in a map--indexed by the
	// field name. Eventually, we might coalesce on a single approach for
	// both kinds of objects.
	if len(superclasses) == 0 && len(k.Data.Fields) == 0 {
		goto runInitializer // check to see if any static initializers
	}

	if len(superclasses) == 0 {
		for i := 0; i < len(k.Data.Fields); i++ {
			f := k.Data.Fields[i]
			desc := k.Data.CP.Utf8Refs[f.Desc]
			name := k.Data.CP.Utf8Refs[f.Name]
			if log.Level == log.FINE {
				reciteField := fmt.Sprintf("Class: %s field[%d] name: %s, type: %s", k.Data.Name, i,
					name, desc)
				_ = log.Log(reciteField, log.FINE)
			}

			fieldToAdd, err := createField(f, k, classname)
			if err != nil {
				return nil, err
			}
			obj.Fields = append(obj.Fields, *fieldToAdd)
		} // loop through the fields if any
		obj.FieldTable = nil
		goto runInitializer
		// return &obj, nil
	} // end of handling fields for objects w/ no superclasses

	obj.FieldTable = make(map[string]object.Field)
	// in the case of superclasses, we start at the topmost superclass
	// and work our way down to the present class, adding fields to FieldTable.
	// so we add the present class into position[0] and then loop through
	// the slice of class names
	superclasses = append([]string{classname}, superclasses...)
	for j := len(superclasses) - 1; j >= 0; j-- {
		superclassName := superclasses[j]
		c := classloader.MethAreaFetch(superclassName)
		if c == nil {
			errMsg := fmt.Sprintf("Error in class instantiation, cannot find superclass: %s",
				superclassName)
			_ = log.Log(errMsg, log.SEVERE)
			return nil, errors.New(errMsg)
		}
		for i := 0; i < len(c.Data.Fields); i++ {
			f := c.Data.Fields[i]
			desc := c.Data.CP.Utf8Refs[f.Desc]
			name := c.Data.CP.Utf8Refs[f.Name]
			if log.Level == log.FINE {
				reciteField := fmt.Sprintf("Class: %s field[%d] name: %s, type: %s", k.Data.Name, i,
					name, desc)
				_ = log.Log(reciteField, log.FINE)
			}

			fieldToAdd, err := createField(f, c, classname)
			if err != nil {
				return nil, err
			}

			// add the field to the field table for this
			obj.FieldTable[name] = *fieldToAdd
		} // end of handling fields for one  class or superclass
	} // end of handling fields for classes with superclasses other than Object

runInitializer:
	// run intialization blocks
	for i := 0; i < len(k.Data.Methods); i++ {
		meth := k.Data.Methods[i]
		methName := k.Data.CP.Utf8Refs[meth.Name]
		if strings.HasPrefix(methName, "<clinit>") {
			err := runInitializationBlock(k, i)
			if err != nil {
				errMsg := fmt.Sprintf("error encountered running %s<clinit>", classname)
				_ = log.Log(errMsg, log.SEVERE)
				return nil, errors.New(errMsg)
			}
		}
	}
	return &obj, nil
}

// Initialization blocks are code blocks that for all intents are methods. They're gathered up by the
// Java compiler into a method called <clinit>, which must be run at class instantiation--that is,
// before any constructor. Because that code might well call other methods, it will need to be run
// just like a regular method with stack frames and depending on the interpreter in run.go
// In addition, we have to make sure that the initialization blocks of superclasses have been
// executed as well.
//
// CURR: Implement the above logic here.
func runInitializationBlock(k *classloader.Klass, idx int) error {

	// msg := fmt.Sprintf("<clinit> found in %s, method #%d\n", k.Data.Name, idx)
	// _ = log.Log(msg, log.FINE)
	// fmt.Print(msg)

	className := k.Data.Name
	me, err := classloader.FetchMethodAndCP(className, "<clinit>", "()V")
	if err != nil {
		return errors.New("Method not found: " + className + "<clinit>()")
	}

	switch me.Meth.(type) {

	}
	// m := me.Meth.(classloader.JmEntry)
	// f := frames.CreateFrame(m.MaxStack) // create a new frame
	// f.MethName = "<clinit>"
	// f.ClName = className
	// f.CP = m.Cp                        // add its pointer to the class CP
	// for i := 0; i < len(m.Code); i++ { // copy the bytecodes over
	// 	f.Meth = append(f.Meth, m.Code[i])
	// }
	//
	// // allocate the local variables
	// for j := 0; j < m.MaxLocals; j++ {
	// 	f.Locals = append(f.Locals, 0)
	// }
	//
	// // create the first thread and place its first frame on it
	// glob := globals.GetGlobalRef()
	// clInitThread := thread.CreateThread()
	// clInitThread.Stack = frames.CreateFrameStack()
	// clInitThread.ID = thread.AddThreadToTable(&clInitThread, &glob.Threads)
	//
	// clInitThread.Trace = MainThread.Trace
	// f.Thread = clInitThread.ID
	//
	// if frames.PushFrame(clInitThread.Stack, f) != nil {
	// 	_ = log.Log("Memory exceptions allocating frame on thread: "+strconv.Itoa(clInitThread.ID),
	// 		log.SEVERE)
	// 	return errors.New("outOfMemory Exception")
	// }
	//
	// if clInitThread.Trace {
	// 	traceInfo := fmt.Sprintf("StartExec: f.MethName=%s, m.MaxStack=%d, m.MaxLocals=%d, len(m.Code)=%d",
	// 		f.MethName, m.MaxStack, m.MaxLocals, len(m.Code))
	// 	_ = log.Log(traceInfo, log.TRACE_INST)
	// }
	//
	// err = runThread(&clInitThread)
	// if err != nil {
	// 	return err
	// }
	return nil
}

// creates a field for insertion into the object representation
func createField(f classloader.Field, k *classloader.Klass, classname string) (*object.Field, error) {
	desc := k.Data.CP.Utf8Refs[f.Desc]
	name := k.Data.CP.Utf8Refs[f.Name]
	if log.Level == log.FINE {
		reciteField := fmt.Sprintf("Class: %s field name: %s, type: %s", k.Data.Name, name, desc)
		_ = log.Log(reciteField, log.FINE)
	}

	fieldToAdd := new(object.Field)
	fieldToAdd.Ftype = desc
	switch string(fieldToAdd.Ftype[0]) {
	case types.Ref, types.Array: // it's a reference
		fieldToAdd.Fvalue = nil
	case types.Byte, types.Char, types.Int, types.Long, types.Short, types.Bool:
		fieldToAdd.Fvalue = int64(0)
	case types.Double, types.Float:
		fieldToAdd.Fvalue = 0.0
	default:
		_ = log.Log("error creating field in: "+classname+
			" Invalid type: "+fieldToAdd.Ftype, log.SEVERE)
		return nil, classloader.CFE("invalid field type")
	}

	presentType := fieldToAdd.Ftype
	if f.IsStatic {
		// in the instantiated class, add an 'X' before the
		// type, which notifies future users that the field
		// is static and should be fetched from the Statics
		// table.
		fieldToAdd.Ftype = "X" + presentType
	}

	// static fields can have ConstantValue attributes,
	// which specify their initial value.
	if len(f.Attributes) > 0 {
		for j := 0; j < len(f.Attributes); j++ {
			attr := k.Data.CP.Utf8Refs[int(f.Attributes[j].AttrName)]
			if attr == "ConstantValue" && f.IsStatic { // only statics can have ConstantValue attribute
				valueIndex := int(f.Attributes[j].AttrContent[0])*256 +
					int(f.Attributes[j].AttrContent[1])
				valueType := k.Data.CP.CpIndex[valueIndex].Type
				valueSlot := k.Data.CP.CpIndex[valueIndex].Slot
				switch valueType {
				case classloader.IntConst:
					fieldToAdd.Fvalue = int64(k.Data.CP.IntConsts[valueSlot])
				case classloader.LongConst:
					fieldToAdd.Fvalue = k.Data.CP.LongConsts[valueSlot]
				case classloader.FloatConst:
					fieldToAdd.Fvalue = float64(k.Data.CP.Floats[valueSlot])
				case classloader.DoubleConst:
					fieldToAdd.Fvalue = k.Data.CP.Doubles[valueSlot]
				case classloader.StringConst:
					str := k.Data.CP.Utf8Refs[valueSlot]
					fieldToAdd.Fvalue = object.NewStringFromGoString(str)
				default:
					errMsg := fmt.Sprintf(
						"Unexpected ConstantValue type in instantiate: %d", valueType)
					_ = log.Log(errMsg, log.SEVERE)
					return nil, errors.New(errMsg)
				} // end of ConstantValue type switch
			} // end of ConstantValue attribute processing
		} // end of processing attributes
	} // end of search through attributes

	if f.IsStatic {
		s := classloader.Static{
			Type:  presentType, // we use the type without the 'X" prefix in the statics table.
			Value: fieldToAdd.Fvalue,
		}
		// add the field to the Statics table
		fieldName := k.Data.CP.Utf8Refs[f.Name]
		fullFieldName := classname + "." + fieldName

		_, alreadyPresent := classloader.Statics[fullFieldName]
		if !alreadyPresent { // add only if field has not been pre-loaded
			_ = classloader.AddStatic(fullFieldName, s)
		}
	}
	return fieldToAdd, nil
}

// Loads the class (if it's not already loaded) and makes sure it's accessible in the method area
func loadThisClass(className string) error {
	alreadyLoaded := classloader.MethAreaFetch(className)
	if alreadyLoaded != nil { // if the class is already loaded, skip the rest of this
		return nil
	}
	// Try to load class by name
	err := classloader.LoadClassFromNameOnly(className)
	if err != nil {
		errMsg := "instantiateClass: Failed to load class " + className
		_ = log.Log(errMsg, log.SEVERE)
		_ = log.Log(err.Error(), log.SEVERE)
		shutdown.Exit(shutdown.APP_EXCEPTION)
		return errors.New(errMsg) // needed for testing, which does not shutdown on failure
	}
	// Success in loaded by name
	_ = log.Log("loadThisClass: Success in LoadClassFromNameOnly("+className+")", log.TRACE_INST)

	// at this point the class has been loaded into the method area (MethArea). Wait for it to be ready.
	err = classloader.WaitForClassStatus(className)
	if err != nil {
		errMsg := fmt.Sprintf("Error occurred in loadThisClass(): %s", err.Error())
		_ = log.Log(errMsg, log.SEVERE)
		return errors.New(errMsg) // needed for testing, which does not shutdown on failure
	}
	return nil
}
