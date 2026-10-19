# Built-in terraform ruleset only, so tflint runs offline (no plugin download).
config {
  call_module_type = "local"
}

plugin "terraform" {
  enabled = true
  preset  = "recommended"
}
