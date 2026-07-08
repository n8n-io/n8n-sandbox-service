output "vm_public_ip" {
  description = "Public IP address of the VM"
  value       = azurerm_public_ip.e2e.ip_address
}

output "vm_name" {
  description = "Name of the VM"
  value       = azurerm_linux_virtual_machine.e2e.name
}

output "vm_private_ip" {
  description = "Private IP address of the primary VM"
  value       = azurerm_network_interface.e2e.private_ip_address
}

output "peer_vm_private_ip" {
  description = "Private IP address of the peer runner VM (empty when peer_vm_enabled is false)"
  value       = var.peer_vm_enabled ? azurerm_network_interface.peer[0].private_ip_address : ""
}

output "peer_vm_public_ip" {
  description = "Public IP address of the peer runner VM (empty when peer_vm_enabled is false)"
  value       = var.peer_vm_enabled ? azurerm_public_ip.peer[0].ip_address : ""
}

output "peer_vm_name" {
  description = "Name of the peer runner VM (empty when peer_vm_enabled is false)"
  value       = var.peer_vm_enabled ? azurerm_linux_virtual_machine.peer[0].name : ""
}
