target "clichat" {
  dockerfile = "images/clichat/Dockerfile"
  tags = [ "clichat:latest" ]
}
 
target "llama-server" {
  dockerfile = "images/llama-server/Dockerfile"
}

target "bartowski" {
  contexts = {
    llama-server = "target:llama-server"
  }
  dockerfile = "images/bartowski/Dockerfile"
  tags = [ "llamacpp-llama3-8b-instruct-bartowski-q5km:latest" ]
}