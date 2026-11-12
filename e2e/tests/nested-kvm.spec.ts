import { test, expect } from '@playwright/test';
import { createSandbox, deleteSandbox, exec, uploadFile } from './helpers';
import { FIRECRACKER_ONLY } from './tags';

// Guest /proc/cpuinfo and /dev/cpu/*/cpuid are unusable; CPUID via mmap (VMX/SVM bits).
const GUEST_NO_HYPERVISOR_CPUID = `
import ctypes
import ctypes.util
import sys

class CPUIDRegs(ctypes.Structure):
    _fields_ = [
        ("eax", ctypes.c_uint32),
        ("ebx", ctypes.c_uint32),
        ("ecx", ctypes.c_uint32),
        ("edx", ctypes.c_uint32),
    ]

# rdi=regs*, esi=leaf, edx=subleaf → fill regs via CPUID
code = bytes([
    0x53, 0x89, 0xF0, 0x89, 0xD1, 0x0F, 0xA2,
    0x89, 0x07, 0x89, 0x5F, 0x04, 0x89, 0x4F, 0x08, 0x89, 0x57, 0x0C,
    0x5B, 0xC3,
])

libc = ctypes.CDLL(ctypes.util.find_library("c"), use_errno=True)
libc.mmap.restype = ctypes.c_void_p
libc.mprotect.argtypes = [ctypes.c_void_p, ctypes.c_size_t, ctypes.c_int]
PROT_READ, PROT_WRITE, PROT_EXEC = 1, 2, 4
MAP_PRIVATE, MAP_ANONYMOUS = 0x02, 0x20
page = 4096
addr = libc.mmap(None, page, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0)
if addr in (0, None, ctypes.c_void_p(-1).value):
    raise SystemExit(f"mmap failed errno={ctypes.get_errno()}")
ctypes.memmove(addr, code, len(code))
if libc.mprotect(addr, page, PROT_READ | PROT_EXEC) != 0:
    raise SystemExit(f"mprotect failed errno={ctypes.get_errno()}")

cpuid = ctypes.CFUNCTYPE(
    None, ctypes.POINTER(CPUIDRegs), ctypes.c_uint32, ctypes.c_uint32
)(addr)
regs = CPUIDRegs()
cpuid(ctypes.byref(regs), 1, 0)
assert not (regs.ecx & (1 << 5)), f"vmx set in leaf1 ecx=0x{regs.ecx:08x}"
cpuid(ctypes.byref(regs), 0x80000001, 0)
assert not (regs.ecx & (1 << 2)), f"svm set in leaf 0x80000001 ecx=0x{regs.ecx:08x}"
print("ok")
`.trim();

// Host nested=0 should keep hypervisor CPU features out of the guest without
// Firecracker CPUID masking.
test.describe('Firecracker nested KVM off', FIRECRACKER_ONLY, () => {
  test('guest CPUID does not advertise VMX or SVM', async () => {
    const id = await createSandbox();
    try {
      await uploadFile(id, '/tmp/check-cpuid.py', GUEST_NO_HYPERVISOR_CPUID + '\n');
      const result = await exec(id, 'python3 /tmp/check-cpuid.py');
      expect(result, result.stdout + result.stderr).toHaveSucceeded();
      expect(result.stdout.trim()).toBe('ok');
    } finally {
      await deleteSandbox(id);
    }
  });
});
