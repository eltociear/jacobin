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
	"jacobin/log"
	"jacobin/object"
	"jacobin/shutdown"
	"strings"
	"unsafe"
)

// instantiating a class is a two-part process (except for arrays, where nothing happens)
//  1. the class needs to be loaded, so that its details and its methods are knowable
//  2. the class fields (if static) and instance fields (if non-static) are allocated. Details
//     for this second step appear at the initializeFields() method.
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
	superclass := k.Data.Superclass
	for {
		if superclass == "java/lang/Object" {
			break
		}

		err := loadThisClass(superclass) // load the superclass
		if err != nil {                  // error message will have been displayed
			return nil, err
		}

		loadedSuperclass := classloader.MethAreaFetch(superclass)
		// now loop to see whether this superclass has a superclass
		superclass = loadedSuperclass.Data.Superclass
	}

	// the object's mark field contains the lower 32-bits of the object's
	// address, which serves as the hash code for the object
	uintp := uintptr(unsafe.Pointer(&obj))
	obj.Mark.Hash = uint32(uintp)

	if len(k.Data.Fields) > 0 {
		for i := 0; i < len(k.Data.Fields); i++ {
			f := k.Data.Fields[i]
			desc := k.Data.CP.Utf8Refs[f.Desc]

			fieldToAdd := new(object.Field)
			fieldToAdd.Ftype = desc
			switch string(fieldToAdd.Ftype[0]) {
			case "L", "[": // it's a reference
				fieldToAdd.Fvalue = nil
			case "B", "C", "I", "J", "S", "Z":
				fieldToAdd.Fvalue = int64(0)
			case "D", "F":
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
			obj.Fields = append(obj.Fields, *fieldToAdd)
		} // loop through the fields if any
	} // test if there are any declared fields

	return &obj, nil
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
		msg := "instantiateClass: Failed to load class " + className
		_ = log.Log(msg, log.SEVERE)
		_ = log.Log(err.Error(), log.SEVERE)
		shutdown.Exit(shutdown.APP_EXCEPTION)
	}
	// Success in loaded by name
	_ = log.Log("instantiateClass: Success in LoadClassFromNameOnly("+className+")", log.TRACE_INST)

	// at this point the class has been loaded into the method area (MethArea). Wait for it to be ready.
	err = classloader.WaitForClassStatus(className)
	if err != nil {
		errMsg := fmt.Sprintf("instantiateClass: %s", err.Error())
		_ = log.Log(errMsg, log.SEVERE)
		return errors.New(errMsg)
	}
	return nil
}
