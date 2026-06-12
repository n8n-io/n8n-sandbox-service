output "deployment_name" {
  value = var.deployment_name
}

output "instance_public_ips" {
  value = azurerm_public_ip.instance[*].ip_address
}

output "instance_names" {
  value = azurerm_linux_virtual_machine.instance[*].name
}

output "instance_count" {
  value = var.instance_count
}

output "ssh_public_key_path" {
  value = var.ssh_public_key_path
}

output "admin_username" {
  value = var.admin_username
}
