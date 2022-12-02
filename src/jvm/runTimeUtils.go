/*
 * Jacobin VM - A Java virtual machine
 * Copyright (c) 2022 by the Jacobin authors. All rights reserved.
 * Licensed under Mozilla Public License 2.0 (MPL 2.0)
 */

package jvm

import (
	"jacobin/classloader"
	"strconv"
	"unsafe"
)

type cpType struct {
	entryType int
	retType   int
	intVal    int64
	floatVal  float64
	addrVal   uintptr
}

var IS_ERROR = 0
var IS_STRUCT_ADDR = 1
var IS_FLOAT64 = 2
var IS_INT64 = 3
var IS_STRING_ADDR = 4

// Utility routines for runtime operations

// Look up an entry in a CP and return its type and its value.
// The returned value is a struct that serves as a substitute for a discriminated
// union.
// The fields are:
//  1. entryType: The CP entry type. Equals 0 if an error occurred.
//     The five entryType values are listed above: IS_ERROR, etc.
//  2. retType: int that identifies the type of the returned value.
//     The options are:
//     0 = error
//     1 = address of item other than string
//     2 = float64
//     3 = int64
//     4 = address of string
//  3. three fields that hold an int64, float64, or 64-bit address, respectively.
//     The calling function checks the retType field to determine which
//     of these three fields holds the returned value.

func FetchCPentry(cpp *classloader.CPool, index int) cpType {
	if cpp == nil {
		return cpType{}
	}
	cp := *cpp
	// if index is out of range, return error
	if index < 1 || index >= len(cp.CpIndex) {
		return cpType{}
	}

	entry := cp.CpIndex[index]

	switch entry.Type {
	// integers
	case classloader.IntConst:
		retInt := int64(cp.IntConsts[entry.Slot])
		return cpType{entryType: IS_INT64, intVal: retInt}

	case classloader.LongConst:
		retInt := cp.LongConsts[entry.Slot]
		return cpType{entryType: IS_INT64, intVal: retInt}

	case classloader.MethodType: // method type is an integer
		retInt := int64(cp.MethodTypes[entry.Slot])
		return cpType{entryType: IS_INT64, intVal: retInt}

	// floating point
	case classloader.FloatConst:
		retFloat := float64(cp.Floats[entry.Slot])
		return cpType{entryType: IS_FLOAT64, floatVal: retFloat}

	case classloader.DoubleConst:
		retFloat := cp.Doubles[entry.Slot]
		return cpType{entryType: IS_FLOAT64, floatVal: retFloat}

	// addresses of strings
	case classloader.ClassRef: // points to a UTF-8 string
		v := unsafe.Pointer(&(cp.Utf8Refs[entry.Slot]))
		return cpType{entryType: IS_STRING_ADDR, addrVal: uintptr(v)}

	case classloader.UTF8: // same code as for ClassRef
		v := unsafe.Pointer(&(cp.Utf8Refs[entry.Slot]))
		return cpType{entryType: IS_STRING_ADDR, addrVal: uintptr(v)}

	// // addresses of structures or other elements
	// case classloader.Dynamic:
	// 	return entry.Type, 1, &(cp.Dynamics[entry.Slot])
	//
	// case classloader.Interface:
	// 	return entry.Type, 1, &(cp.InterfaceRefs[entry.Slot])
	//
	// case classloader.InvokeDynamic:
	// 	return entry.Type, 1, &(cp.InvokeDynamics[entry.Slot])
	//
	// case classloader.MethodHandle:
	// 	return entry.Type, 1, unsafe.Pointer(&(cp.MethodHandles[entry.Slot]))
	//
	// case classloader.MethodRef:
	// 	return entry.Type, 1, unsafe.Pointer(&(cp.MethodRefs[entry.Slot]))
	//
	// case classloader.NameAndType:
	// 	return entry.Type, 1, &(cp.NameAndTypes[entry.Slot])

	// error: name of module or package would
	// not normally be retrieved here
	case classloader.Module,
		classloader.Package:
		return cpType{}
	}

	return cpType{}
}

func UnsafePtrToString(up unsafe.Pointer) string {
	val := int(uintptr(up))
	str := strconv.Itoa(val)
	return str
}
