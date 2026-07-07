variable "cidr" {
type=string
}
resource "aws_subnet" "private" {
cidr_block=var.cidr
}
