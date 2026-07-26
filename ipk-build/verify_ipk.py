#!/usr/bin/env python3
"""Verify ipk package format matches opkg expectations."""
import os
import struct
import gzip
import tarfile
import io

def parse_ar_header(header):
    """Parse a single ar header and validate fields."""
    if len(header) != 60:
        return None, f"Header size mismatch: {len(header)}"
    
    name = header[0:16].decode("ascii", errors="replace").strip()
    mtime = header[16:28].decode("ascii", errors="replace").strip()
    uid = header[28:34].decode("ascii", errors="replace").strip()
    gid = header[34:40].decode("ascii", errors="replace").strip()
    mode = header[40:48].decode("ascii", errors="replace").strip()
    size_str = header[48:58].decode("ascii", errors="replace").strip()
    end = header[58:60]
    
    errors = []
    
    # Validate name
    if not name:
        errors.append("Empty name")
    
    # Validate numeric fields are right-aligned (should not have leading spaces in the value)
    # Actually in ar format, fields are right-aligned with space padding
    # So the stripped value should be a valid number
    try:
        int(mtime)
    except ValueError:
        errors.append(f"Invalid mtime: {mtime!r}")
    
    try:
        int(uid)
    except ValueError:
        errors.append(f"Invalid uid: {uid!r}")
    
    try:
        int(gid)
    except ValueError:
        errors.append(f"Invalid gid: {gid!r}")
    
    # Mode should be octal
    if not mode.startswith("1") and not mode.startswith("0"):
        errors.append(f"Invalid mode prefix: {mode!r}")
    try:
        int(mode, 8)
    except ValueError:
        errors.append(f"Invalid octal mode: {mode!r}")
    
    try:
        size = int(size_str)
    except ValueError:
        errors.append(f"Invalid size: {size_str!r}")
        size = 0
    
    if end != b"`\n":
        errors.append(f"Invalid end marker: {end!r}")
    
    return {
        "name": name,
        "mtime": mtime,
        "uid": uid,
        "gid": gid,
        "mode": mode,
        "size": size,
        "errors": errors
    }, None

def main():
    fname = r"C:\Users\Administrator\Downloads\model-gateway\luci-app-model-gateway\luci-app-model-gateway_1.0.0-r20260722_all.ipk"
    
    with open(fname, "rb") as f:
        magic = f.read(8)
        print(f"=== Global Magic ===")
        print(f"  {magic!r}")
        if magic != b"!<arch>\n":
            print("  ERROR: Invalid ar magic!")
            return
        print("  OK")
        
        members = []
        while True:
            header = f.read(60)
            if len(header) < 60:
                break
            
            info, err = parse_ar_header(header)
            if err:
                print(f"\n=== Member Parse Error ===")
                print(f"  {err}")
                return
            
            print(f"\n=== Member: {info['name']} ===")
            print(f"  size: {info['size']}")
            print(f"  mtime: {info['mtime']}")
            print(f"  uid: {info['uid']}")
            print(f"  gid: {info['gid']}")
            print(f"  mode: {info['mode']}")
            
            if info['errors']:
                for e in info['errors']:
                    print(f"  ERROR: {e}")
            else:
                print("  Header: OK")
            
            content = f.read(info['size'])
            if len(content) != info['size']:
                print(f"  ERROR: Expected {info['size']} bytes, got {len(content)}")
                return
            
            # Skip padding
            if info['size'] % 2 != 0:
                f.read(1)
            
            members.append((info['name'], content))
    
    print(f"\n=== Total members: {len(members)} ===")
    
    # Verify expected members
    expected = ["debian-binary", "control.tar.gz", "data.tar.gz"]
    actual = [m[0] for m in members]
    
    if actual == expected:
        print("  Member order: OK")
    else:
        print(f"  WARNING: Expected {expected}, got {actual}")
    
    # Verify debian-binary content
    debian_content = members[0][1]
    if debian_content == b"2.0\n":
        print("  debian-binary: OK (2.0)")
    else:
        print(f"  debian-binary: WARNING ({debian_content!r})")
    
    # Verify control.tar.gz
    control_content = members[1][1]
    try:
        with gzip.GzipFile(fileobj=io.BytesIO(control_content)) as gz:
            with tarfile.open(fileobj=gz, mode="r") as tf:
                names = [m.name for m in tf.getmembers()]
                print(f"  control.tar.gz: OK")
                print(f"    files: {names}")
    except Exception as e:
        print(f"  control.tar.gz: ERROR - {e}")
    
    # Verify data.tar.gz
    data_content = members[2][1]
    try:
        with gzip.GzipFile(fileobj=io.BytesIO(data_content)) as gz:
            with tarfile.open(fileobj=gz, mode="r") as tf:
                names = [m.name for m in tf.getmembers()]
                print(f"  data.tar.gz: OK")
                print(f"    files: {len(names)} entries")
                for name in names:
                    print(f"      - {name}")
    except Exception as e:
        print(f"  data.tar.gz: ERROR - {e}")

if __name__ == "__main__":
    main()
