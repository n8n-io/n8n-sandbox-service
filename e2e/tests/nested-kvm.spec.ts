import { test, expect } from '@playwright/test';
import { createSandbox, deleteSandbox, exec } from './helpers';
import { FIRECRACKER_ONLY } from './tags';

// Firecracker guests may have an empty /proc (no cpuinfo). Read CPUID via the
// kernel cpuid device instead: leaf 1 ECX bit 5 = VMX, leaf 0x80000001 ECX bit 2 = SVM.
const GUEST_NO_HYPERVISOR_CPUID =
  'python3 -c \'' +
  'import struct;' +
  'f=open("/dev/cpu/0/cpuid","rb");' +
  'f.seek(1);_,_,c,_=struct.unpack("4I",f.read(16));' +
  'assert not (c & (1 << 5)), "vmx";' +
  'f.seek(0x80000001);_,_,c,_=struct.unpack("4I",f.read(16));' +
  'assert not (c & (1 << 2)), "svm";' +
  'print("ok")' +
  '\'';

// Host nested=0 should keep hypervisor CPU features out of the guest without
// Firecracker CPUID masking.
test.describe('Firecracker nested KVM off', FIRECRACKER_ONLY, () => {
  test('guest CPUID does not advertise VMX or SVM', async () => {
    const id = await createSandbox();
    try {
      const result = await exec(id, GUEST_NO_HYPERVISOR_CPUID);
      expect(result, result.stdout + result.stderr).toHaveSucceeded();
      expect(result.stdout.trim()).toBe('ok');
    } finally {
      await deleteSandbox(id);
    }
  });
});
