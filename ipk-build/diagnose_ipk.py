#!/usr/bin/env python3
"""Simulate opkg ipk parsing to find the exact failure point."""
import os
import struct
import gzip
import tarfile
import io

def parse_ar(filename):
    """Parse ar archive and return members."""
    members = []
    with open(filename, "rb") as f:
        magic = f.read(8)
        if magic != b"!<arch>\n":
            raise ValueError(f"Invalid ar magic: {magic!r}")
        
        while True:
            header = f.read(60)
            if len(header) < 60:
                break
            
            name = header[0:16].decode("ascii", errors="replace").strip()
            mtime = header[16:28].decode("ascii", errors="replace").strip()
            uid = header[28:34].decode("ascii", errors="replace").strip()
            gid = header[34:40].decode("ascii", errors="replace").strip()
            mode = header[40:48].decode("ascii", errors="replace").strip()
            size_str = header[48:58].decode("ascii", errors="replace").strip()
            end = header[58:60]
            
            if end != b"`\n":
                raise ValueError(f"Invalid end marker: {end!r}")
            
            try:
                size = int(size_str)
            except ValueError:
                raise ValueError(f"Invalid size: {size_str!r}")
            
            content = f.read(size)
            if len(content) != size:
                raise ValueError(f"Truncated member: {name}")
            
            if size % 2 != 0:
                f.read(1)
            
            members.append({
                "name": name,
                "size": size,
                "content": content
            })
    
    return members

def parse_control(control_data):
    """Parse control file."""
    control = {}
    lines = control_data.decode("utf-8", errors="replace").split("\n")
    current_key = None
    current_value = ""
    
    for line in lines:
        if not line or line.startswith("#"):
            continue
        if line[0] == " " or line[0] == "\t":
            if current_key:
                current_value += "\n" + line.strip()
        else:
            if ":" in line:
                key, _, value = line.partition(":")
                key = key.strip()
                value = value.strip()
                if current_key:
                    control[current_key] = current_value
                current_key = key
                current_value = value
    
    if current_key:
        control[current_key] = current_value
    
    return control

def main():
    ipk = r"C:\Users\Administrator\Downloads\model-gateway\luci-app-model-gateway\ipk-build\luci-app-model-gateway_1.0.0-r20260722_all.ipk"
    
    print("=== Step 1: Parse ar archive ===")
    try:
        members = parse_ar(ipk)
        print(f"  OK: Found {len(members)} members")
        for m in members:
            print(f"    - {m['name']}: {m['size']} bytes")
    except Exception as e:
        print(f"  FAIL: {e}")
        return
    
    print("\n=== Step 2: Verify member order ===")
    expected_order = ["debian-binary", "control.tar.gz", "data.tar.gz"]
    actual_order = [m["name"] for m in members]
    if actual_order == expected_order:
        print("  OK: Member order correct")
    else:
        print(f"  FAIL: Expected {expected_order}, got {actual_order}")
    
    print("\n=== Step 3: Verify debian-binary ===")
    debian = members[0]["content"]
    if debian == b"2.0\n":
        print(f"  OK: debian-binary = {debian!r}")
    else:
        print(f"  FAIL: debian-binary = {debian!r}")
    
    print("\n=== Step 4: Parse control.tar.gz ===")
    control_member = members[1]
    try:
        with gzip.GzipFile(fileobj=io.BytesIO(control_member["content"])) as gz:
            with tarfile.open(fileobj=gz, mode="r") as tf:
                tar_members = tf.getmembers()
                print(f"  OK: control.tar.gz has {len(tar_members)} entries")
                for tm in tar_members:
                    print(f"    - {tm.name} ({tm.size} bytes)")
                
                # Read control file
                control_data = None
                for tm in tar_members:
                    if tm.name == "./control" or tm.name == "control":
                        f = tf.extractfile(tm)
                        control_data = f.read()
                        break
                
                if control_data:
                    print(f"\n  Control file content ({len(control_data)} bytes):")
                    print(f"    {control_data[:200]!r}")
                    
                    control = parse_control(control_data)
                    print(f"\n  Parsed control fields:")
                    for k, v in control.items():
                        print(f"    {k}: {v[:50] if len(v) > 50 else v}")
                    
                    # Check required fields
                    required = ["Package", "Version", "Architecture", "Description"]
                    for field in required:
                        if field in control:
                            print(f"    {field}: OK")
                        else:
                            print(f"    {field}: MISSING!")
                else:
                    print("  FAIL: No control file found")
    except Exception as e:
        print(f"  FAIL: {e}")
        import traceback
        traceback.print_exc()
    
    print("\n=== Step 5: Parse data.tar.gz ===")
    data_member = members[2]
    try:
        with gzip.GzipFile(fileobj=io.BytesIO(data_member["content"])) as gz:
            with tarfile.open(fileobj=gz, mode="r") as tf:
                tar_members = tf.getmembers()
                print(f"  OK: data.tar.gz has {len(tar_members)} entries")
                
                # Check for required files
                required_files = [
                    "./usr/bin/model-gatewayd",
                    "./etc/init.d/model-gateway",
                    "./etc/config/model-gateway",
                    "./usr/lib/lua/luci/controller/model-gateway.lua",
                ]
                for req in required_files:
                    found = any(tm.name == req for tm in tar_members)
                    if found:
                        print(f"    {req}: OK")
                    else:
                        print(f"    {req}: MISSING!")
    except Exception as e:
        print(f"  FAIL: {e}")
        import traceback
        traceback.print_exc()
    
    print("\n=== Step 6: Check for common ipk issues ===")
    
    # Check if data.tar.gz has absolute paths
    data_member = members[2]
    with gzip.GzipFile(fileobj=io.BytesIO(data_member["content"])) as gz:
        with tarfile.open(fileobj=gz, mode="r") as tf:
            for tm in tf.getmembers():
                if tm.name.startswith("/"):
                    print(f"  WARNING: Absolute path in data.tar.gz: {tm.name}")
                if "\\" in tm.name:
                    print(f"  WARNING: Windows path separator: {tm.name}")
    
    print("\n=== Diagnosis complete ===")

if __name__ == "__main__":
    main()
