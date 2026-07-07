variable "cidr" {
  type = string
}

variable "instance_count" {
  type    = number
  default = 2
}

# The AWS region to deploy into.
variable "region" {
  type    = string
  default = "eu-west-1"
}
