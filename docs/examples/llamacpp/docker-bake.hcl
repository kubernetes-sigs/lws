target "clichat" {
  dockerfile = "images/clichat/Dockerfile"
  tags = [ "clichat:latest" ]
}
 
target "llamacpp-leader" {
  dockerfile = "images/llamacpp-leader/Dockerfile"
}

target "llamacpp-worker" {
  dockerfile = "images/llamacpp-worker/Dockerfile"
  tags = [ "llamacpp-worker:latest" ]
}

target "bartowski" {
  contexts = {
    llamacpp-leader = "target:llamacpp-leader"
  }
  dockerfile = "images/bartowski/Dockerfile"
  tags = [ "llamacpp-llama3-8b-instruct-bartowski-q5km:latest" ]
}