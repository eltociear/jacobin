/*
 * Jacobin VM - A Java virtual machine
 * Copyright (c) 2021 by Andrew Binstock. All rights reserved.
 * Licensed under Mozilla Public License 2.0 (MPL 2.0)
 */

package classloader

import "strconv"

// various utilities frequently used in parsing classfiles

// read two bytes in big endian order and convert to an int
func intFrom2Bytes(bytes []byte, pos int) (int, error) {
	if len(bytes) < pos+2 {
		return 0, cfe("invalid offset into file")
	}

	value := (uint16(bytes[pos]) << 8) + uint16(bytes[pos+1])
	return int(value), nil
}

// read four bytes in big endian order and convert to an int
func intFrom4Bytes(bytes []byte, pos int) (int, error) {
	if len(bytes) < pos+4 {
		return 0, cfe("invalid offset into file")
	}

	value1 := (uint32(bytes[pos]) << 8) + uint32(bytes[pos+1])
	value2 := (uint32(bytes[pos+2]) << 8) + uint32(bytes[pos+3])
	retVal := int(value1<<16) + int(value2)
	return retVal, nil
}

// finds and returns a UTF8 string when handed an index into the CP that points
// to a UTF8 entry. Does extensive checking of values.
func fetchUTF8string(klass *parsedClass, index int) (string, error) {
	if index < 1 || index > klass.cpCount-1 {
		return "", cfe("attempt to fetch invalid UTF8 at CP entry #" + strconv.Itoa(index))
	}

	if klass.cpIndex[index].entryType != UTF8 {
		return "", cfe("attempt to fetch UTF8 string from non-UTF8 CP entry #" + strconv.Itoa(index))
	}

	i := klass.cpIndex[index].slot
	if i < 0 || i > len(klass.utf8Refs)-1 {
		return "", cfe("invalid index into UTF8 array of CP: " + strconv.Itoa(i))
	}

	return klass.utf8Refs[i].content, nil
}

// like the preceding function, except this returns the slot number in the utf8Refs
// rather than the string that's in that slot.
func fetchUTF8slot(klass *parsedClass, index int) (int, error) {
	if index < 1 || index > klass.cpCount-1 {
		return -1, cfe("attempt to fetch invalid UTF8 at CP entry #" + strconv.Itoa(index))
	}

	if klass.cpIndex[index].entryType != UTF8 {
		return -1, cfe("attempt to fetch UTF8 string from non-UTF8 CP entry #" + strconv.Itoa(index))
	}

	slot := klass.cpIndex[index].slot
	if slot < 0 || slot > len(klass.utf8Refs)-1 {
		return -1, cfe("invalid index into UTF8 array of CP: " + strconv.Itoa(slot))
	}
	return slot, nil
}

// fetches attribute info. Attributes are values associated with fields, methods, classes, and
// code attributes (yes, the word 'attribute' is overloaded in JVM parlance). The spec is here:
// https://docs.oracle.com/javase/specs/jvms/se11/html/jvms-4.html#jvms-4.7 and the general
// layout is:
// attribute_info {
//    u2 attribute_name_index;  // the name of the attribute
//    u4 attribute_length;
//    u1 info[attribute_length];
// }
func fetchAttribute(klass *parsedClass, bytes []byte, loc int) (attr, int, error) {
	pos := loc
	attribute := attr{}
	nameIndex, err := intFrom2Bytes(bytes, pos+1)
	pos += 2
	if err != nil {
		return attribute, pos, cfe("error fetching field attribute")
	}
	nameSlot, err := fetchUTF8slot(klass, nameIndex)
	if err != nil {
		return attribute, pos, cfe("error fetching name of field attribute")
	}

	attribute.attrName = nameSlot // slot in UTF-8 slice of CP

	length, err := intFrom4Bytes(bytes, pos+1)
	pos += 4
	if err != nil {
		return attribute, pos, cfe("error fetching lenght of field attribute")
	}
	attribute.attrSize = length

	b := make([]byte, length)
	for i := 0; i < length; i++ {
		b[i] = bytes[pos+1+i]
	}

	attribute.attrContent = b

	return attribute, pos + length, nil
}
