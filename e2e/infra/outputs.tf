output "vm_public_ip" {
  description = "Public IP address of the VM"
  value       = azurerm_public_ip.e2e.ip_address
}

output "vm_name" {
  description = "Name of the VM"
  value       = azurerm_linux_virtual_machine.e2e.name
}
