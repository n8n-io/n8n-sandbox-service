# Optional second VM on the same subnet for Firecracker two-runner e2e.
# Enable with peer_vm_enabled = true in e2e-vm.auto.tfvars.json.

resource "azurerm_public_ip" "peer" {
  count               = var.peer_vm_enabled ? 1 : 0
  name                = "${var.vm_name}-peer-pip"
  location            = var.location
  resource_group_name = data.azurerm_resource_group.e2e.name
  allocation_method   = "Static"
  sku                 = "Standard"
  tags                = local.tags
}

resource "azurerm_network_interface" "peer" {
  count               = var.peer_vm_enabled ? 1 : 0
  name                = "${var.vm_name}-peer-nic"
  location            = var.location
  resource_group_name = data.azurerm_resource_group.e2e.name
  tags                = local.tags

  ip_configuration {
    name                          = "internal"
    subnet_id                     = azurerm_subnet.e2e.id
    private_ip_address_allocation = "Dynamic"
    public_ip_address_id          = azurerm_public_ip.peer[0].id
  }
}

resource "azurerm_linux_virtual_machine" "peer" {
  count               = var.peer_vm_enabled ? 1 : 0
  name                = "${var.vm_name}-peer"
  resource_group_name = data.azurerm_resource_group.e2e.name
  location            = var.location
  size                = var.vm_size
  admin_username      = var.admin_username

  network_interface_ids = [
    azurerm_network_interface.peer[0].id,
  ]

  admin_ssh_key {
    username   = var.admin_username
    public_key = file(var.ssh_public_key_path)
  }

  os_disk {
    caching              = "ReadWrite"
    storage_account_type = "Premium_LRS"
    disk_size_gb         = var.os_disk_size_gb
  }

  source_image_reference {
    publisher = "Canonical"
    offer     = "ubuntu-24_04-lts"
    sku       = "server"
    version   = "latest"
  }

  tags = local.tags
}

resource "azurerm_network_interface_security_group_association" "peer" {
  count                     = var.peer_vm_enabled ? 1 : 0
  network_interface_id      = azurerm_network_interface.peer[0].id
  network_security_group_id = azurerm_network_security_group.e2e.id
}
