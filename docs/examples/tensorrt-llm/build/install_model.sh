# !bin/bash
mkdir /data
cd /data
git clone https://github.com/triton-inference-server/tensorrtllm_backend.git -b v0.11.0
cd tensorrtllm_backend
git lfs install
git submodule update --init --recursive

cd tensorrt_llm/examples/llama
pip install huggingface_hub==0.24.6
huggingface-cli download meta-llama/Meta-Llama-3-70B --local-dir-use-symlinks=False --local-dir=./Meta-Llama-3-70B

python3 convert_checkpoint.py --model_dir ./Meta-Llama-3-70B \
                             --output_dir ./converted_checkpoint \
                             --dtype float16 \
                             --tp_size 8 \
                             --pp_size 2

trtllm-build --checkpoint_dir ./converted_checkpoint \
             --output_dir ./output_engines \
             --gemm_plugin float16 \
             --use_custom_all_reduce disable \
             --max_input_len 2048 \
             --max_output_len 2048 \
             --max_batch_size 4

cd /data/tensorrtllm_backend
rm -rf /data/tensorrtllm_backend/all_models/inflight_batcher_llm/tensorrt_llm_bls/

python3 tools/fill_template.py -i all_models/inflight_batcher_llm/preprocessing/config.pbtxt tokenizer_dir:/data/tensorrtllm_backend/tensorrt_llm/examples/llama/Meta-Llama-3-70B/,tokenizer_type:llama,triton_max_batch_size:4,preprocessing_instance_count:1
python3 tools/fill_template.py -i all_models/inflight_batcher_llm/tensorrt_llm/config.pbtxt triton_backend:tensorrtllm,triton_max_batch_size:4,decoupled_mode:True,max_beam_width:1,engine_dir:/data/tensorrtllm_backend/tensorrt_llm/examples/llama/output_engines/,enable_kv_cache_reuse:False,batching_strategy:inflight_batching,max_queue_delay_microseconds:0
python3 tools/fill_template.py -i all_models/inflight_batcher_llm/postprocessing/config.pbtxt tokenizer_dir:/data/tensorrtllm_backend/tensorrt_llm/examples/llama/Meta-Llama-3-70B/,tokenizer_type:llama,triton_max_batch_size:4,postprocessing_instance_count:1
python3 tools/fill_template.py -i all_models/inflight_batcher_llm/ensemble/config.pbtxt triton_max_batch_size:4
             