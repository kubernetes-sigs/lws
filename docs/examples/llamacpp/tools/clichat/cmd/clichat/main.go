package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"k8s.io/klog/v2"
)

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	stream := true
	llmEndpoint := os.Getenv("LLM_ENDPOINT")
	if llmEndpoint == "" {
		llmEndpoint = "http://127.0.0.1:8080" // port-forward default
	}
	userSays := ""

	flag.StringVar(&llmEndpoint, "llm-endpoint", llmEndpoint, "llama.cpp endpoint to connect to")
	flag.StringVar(&userSays, "user", userSays, "Initial prompt to use")

	klog.InitFlags(nil)

	flag.Parse()

	client, err := NewLllamaCPPClient(llmEndpoint)
	if err != nil {
		return err
	}

	reader := bufio.NewReader(os.Stdin)
	for {
		if userSays == "" {
			fmt.Print("\n> ")
			line, err := reader.ReadString('\n')
			if err != nil {
				if err == io.EOF {
					break
				}
				return fmt.Errorf("reading user text: %w", err)
			}
			userSays = line
		}

		prompt := `
		<|begin_of_text|><|start_header_id|>system<|end_header_id|>
		
		{system_prompt}<|eot_id|><|start_header_id|>user<|end_header_id|>
		
		{prompt}<|eot_id|><|start_header_id|>assistant<|end_header_id|>
		`

		prompt = strings.ReplaceAll(prompt, "{system_prompt}", "You are a helpful AI assistant.  Answer concisely and directly.")
		prompt = strings.ReplaceAll(prompt, "{prompt}", userSays)

		req := &completionRequest{
			Prompt:     prompt,
			Stream:     true,
			NumPredict: PtrTo(1024),
		}

		if stream {
			if err := client.doCompletionStreaming(ctx, req, func(response *completionResponse) error {
				fmt.Fprintf(os.Stdout, "%v", response.Content)
				return nil
			}); err != nil {
				return fmt.Errorf("generating with llama.cpp: %w", err)
			}

		} else {
			response, err := client.doCompletion(ctx, req)
			if err != nil {
				return fmt.Errorf("generating with llama.cpp: %w", err)
			}
			klog.V(2).Infof("response is %+v", response)

			fmt.Fprintf(os.Stdout, "%v\n", response.Content)
		}

		userSays = ""
	}

	return nil
}

func PtrTo[T any](t T) *T {
	return &t
}

type llamacppClient struct {
	httpClient *http.Client
	baseURL    string
}

func NewLllamaCPPClient(baseURL string) (*llamacppClient, error) {
	httpClient := &http.Client{
		Timeout: 5 * time.Second,
	}
	return &llamacppClient{httpClient: httpClient, baseURL: baseURL}, nil
}

func (c *llamacppClient) doCompletion(ctx context.Context, request *completionRequest) (*completionResponse, error) {
	endpoint, err := url.JoinPath(c.baseURL, "/completion")
	if err != nil {
		return nil, fmt.Errorf("joining completion endpoint: %w", err)
	}
	response, err := doHTTPRequestAndParse[completionResponse](ctx, c.httpClient, "POST", endpoint, request)
	if err != nil {
		return nil, fmt.Errorf("calling llama.cpp completion: %w", err)
	}
	return response, nil
}

func (c *llamacppClient) doCompletionStreaming(ctx context.Context, request *completionRequest, callback func(response *completionResponse) error) error {
	endpoint, err := url.JoinPath(c.baseURL, "/completion")
	if err != nil {
		return fmt.Errorf("joining completion endpoint: %w", err)
	}

	resp, err := doHTTPRequest(ctx, c.httpClient, "POST", endpoint, request)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code from completion request: %s", resp.Status)
	}

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return fmt.Errorf("reading completion response: %w", err)
		}

		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		klog.V(2).Infof("response is %v", string(line))
		if bytes.HasPrefix(line, []byte("data: ")) {
			line = line[5:]
			var response completionResponse
			if err := json.Unmarshal(line, &response); err != nil {
				return fmt.Errorf("unmarshalling response body: %w", err)
			}
			klog.V(2).Infof("response is %+v", response)
			if err := callback(&response); err != nil {
				return err
			}
			if response.Stop {
				break
			}
		} else {
			return fmt.Errorf("unexpected response line: %q", string(line))
		}
	}

	return nil
}

type completionRequest struct {
	// 	prompt: Provide the prompt for this completion as a string or as an array of strings or numbers representing tokens. Internally, if cache_prompt is true, the prompt is compared to the previous completion and only the "unseen" suffix is evaluated. A BOS token is inserted at the start, if all of the following conditions are true:

	// - The prompt is a string or an array with the first element given as a string
	// - The model's `tokenizer.ggml.add_bos_token` metadata is `true`
	// - The system prompt is empty
	Prompt string `json:"prompt,omitempty"`

	// temperature: Adjust the randomness of the generated text. Default: 0.8
	Temperature float64 `json:"temperature"`

	// dynatemp_range: Dynamic temperature range. The final temperature will be in the range of [temperature - dynatemp_range; temperature + dynatemp_range] Default: 0.0, which is disabled.

	// dynatemp_exponent: Dynamic temperature exponent. Default: 1.0

	// top_k: Limit the next token selection to the K most probable tokens. Default: 40
	TopK int `json:"top_k"`

	// top_p: Limit the next token selection to a subset of tokens with a cumulative probability above a threshold P. Default: 0.95
	TopP float64 `json:"top_p"`

	// min_p: The minimum probability for a token to be considered, relative to the probability of the most likely token. Default: 0.05
	MinP float64 `json:"min_p,omitempty"`

	// n_predict: Set the maximum number of tokens to predict when generating text.
	// Note: May exceed the set limit slightly if the last token is a partial multibyte character.
	// When 0, no tokens will be generated but the prompt is evaluated into the cache. Default: -1, where -1 is infinity.
	NumPredict *int `json:"n_predict"`

	// n_keep: Specify the number of tokens from the prompt to retain when the context size is exceeded and tokens need to be discarded. By default, this value is set to 0, meaning no tokens are kept. Use -1 to retain all tokens from the prompt.
	NumKeep int `json:"n_keep"`

	// stream: It allows receiving each predicted token in real-time instead of waiting for the completion to finish. To enable this, set to true.
	Stream bool `json:"stream"`

	// stop: Specify a JSON array of stopping strings. These words will not be included in the completion, so make sure to add them to the prompt for the next iteration. Default: []
	Stop []string `json:"stop,omitempty"`

	// tfs_z: Enable tail free sampling with parameter z. Default: 1.0, which is disabled.
	TFSZ float64 `json:"tfs_z"`

	// typical_p: Enable locally typical sampling with parameter p. Default: 1.0, which is disabled.
	TypicalP float64 `json:"typical_p"`

	// repeat_penalty: Control the repetition of token sequences in the generated text. Default: 1.1
	RepeatPenalty float64 `json:"repeat_penalty"`

	// repeat_last_n: Last n tokens to consider for penalizing repetition. Default: 64, where 0 is disabled and -1 is ctx-size.
	RepeatLastN int `json:"repeat_last_n"`

	// penalize_nl: Penalize newline tokens when applying the repeat penalty. Default: true
	PenalizeNewline bool `json:"penalize_nl"`

	// presence_penalty: Repeat alpha presence penalty. Default: 0.0, which is disabled.
	PresencePenalty float64 `json:"presence_penalty"`

	// frequency_penalty: Repeat alpha frequency penalty. Default: 0.0, which is disabled.
	FrequencyPenalty float64 `json:"frequency_penalty"`

	// penalty_prompt: This will replace the prompt for the purpose of the penalty evaluation. Can be either null, a string or an array of numbers representing tokens. Default: null, which is to use the original prompt.

	// mirostat: Enable Mirostat sampling, controlling perplexity during text generation. Default: 0, where 0 is disabled, 1 is Mirostat, and 2 is Mirostat 2.0.
	Mirostat int `json:"mirostat"`

	// mirostat_tau: Set the Mirostat target entropy, parameter tau. Default: 5.0
	MirostatTau float64 `json:"mirostat_tau,omitempty"`

	// mirostat_eta: Set the Mirostat learning rate, parameter eta. Default: 0.1
	MirostatEta float64 `json:"mirostat_eta,omitempty"`

	// grammar: Set grammar for grammar-based sampling. Default: no grammar
	Grammar string `json:"grammar,omitempty"`

	// json_schema: Set a JSON schema for grammar-based sampling (e.g. {"items": {"type": "string"}, "minItems": 10, "maxItems": 100} of a list of strings, or {} for any JSON). See tests for supported features. Default: no JSON schema.
	JSONSchema string `json:"json_schema,omitempty"`

	// seed: Set the random number generator (RNG) seed. Default: -1, which is a random seed.
	Seed int64 `json:"seed,omitempty"`

	// ignore_eos: Ignore end of stream token and continue generating. Default: false

	// logit_bias: Modify the likelihood of a token appearing in the generated text completion. For example, use "logit_bias": [[15043,1.0]] to increase the likelihood of the token 'Hello', or "logit_bias": [[15043,-1.0]] to decrease its likelihood. Setting the value to false, "logit_bias": [[15043,false]] ensures that the token Hello is never produced. The tokens can also be represented as strings, e.g. [["Hello, World!",-0.5]] will reduce the likelihood of all the individual tokens that represent the string Hello, World!, just like the presence_penalty does. Default: []

	// n_probs: If greater than 0, the response also contains the probabilities of top N tokens for each generated token given the sampling settings. Note that for temperature < 0 the tokens are sampled greedily but token probabilities are still being calculated via a simple softmax of the logits without considering any other sampler settings. Default: 0
	NProbs int64 `json:"n_probs,omitempty"`

	// min_keep: If greater than 0, force samplers to return N possible tokens at minimum. Default: 0

	// image_data: An array of objects to hold base64-encoded image data and its ids to be reference in prompt. You can determine the place of the image in the prompt as in the following: USER:[img-12]Describe the image in detail.\nASSISTANT:. In this case, [img-12] will be replaced by the embeddings of the image with id 12 in the following image_data array: {..., "image_data": [{"data": "<BASE64_STRING>", "id": 12}]}. Use image_data only with multimodal models, e.g., LLaVA.

	// id_slot: Assign the completion task to an specific slot. If is -1 the task will be assigned to a Idle slot. Default: -1
	IDSlot int64 `json:"id_slot,omitempty"`

	// cache_prompt: Reuse KV cache from a previous request if possible. This way the common prefix does not have to be re-processed, only the suffix that differs between the requests. Because (depending on the backend) the logits are not guaranteed to be bit-for-bit identical for different batch sizes (prompt processing vs. token generation) enabling this option can cause nondeterministic results. Default: false
	CachePrompt bool `json:"cache_prompt"`

	// system_prompt: Change the system prompt (initial prompt of all slots), this is useful for chat applications. See more
	SystemPrompt *string `json:"system_prompt,omitempty"`

	// samplers: The order the samplers should be applied in. An array of strings representing sampler type names. If a sampler is not set, it will not be used. If a sampler is specified more than once, it will be applied multiple times. Default: ["top_k", "tfs_z", "typical_p", "top_p", "min_p", "temperature"] - these are all the available values.

}

type completionResponse struct {
	// 	content: Completion result as a string (excluding stopping_word if any). In case of streaming mode, will contain the next token as a string.
	Content string `json:"content"`

	// stop: Boolean for use with stream to check whether the generation has stopped (Note: This is not related to stopping words array stop from input options)
	Stop bool `json:"stop"`

	// generation_settings: The provided options above excluding prompt but including n_ctx, model. These options may differ from the original ones in some way (e.g. bad values filtered out, strings converted to tokens, etc.).

	// model: The path to the model loaded with -m
	Model string `json:"model"`

	// prompt: The provided prompt

	// stopped_eos: Indicating whether the completion has stopped because it encountered the EOS token
	StoppedEOS bool `json:"stopped_eos"`

	// stopped_limit: Indicating whether the completion stopped because n_predict tokens were generated before stop words or EOS was encountered
	StoppedLimit bool `json:"stopped_limit"`

	// stopped_word: Indicating whether the completion stopped due to encountering a stopping word from stop JSON array provided
	StoppedWord bool `json:"stopped_word"`

	// stopping_word: The stopping word encountered which stopped the generation (or "" if not stopped due to a stopping word)
	StoppingWord string `json:"stopping_word"`

	// timings: Hash of timing information about the completion such as the number of tokens predicted_per_second
	Timings map[string]float64 `json:"timings"`

	// tokens_cached: Number of tokens from the prompt which could be reused from previous completion (n_past)
	TokensCached int64 `json:"tokens_cached"`

	// tokens_evaluated: Number of tokens evaluated in total from the prompt
	TokensEvaluated int64 `json:"tokens_evaluated"`

	// truncated: Boolean indicating if the context size was exceeded during generation, i.e. the number of tokens provided in the prompt (tokens_evaluated) plus tokens generated (tokens predicted) exceeded the context size (n_ctx)
	Truncated bool `json:"truncated"`
}

func doHTTPRequestAndParse[T any](ctx context.Context, httpClient *http.Client, method string, url string, body any) (*T, error) {
	resp, err := doHTTPRequest(ctx, httpClient, method, url, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code from %v %v: %s", method, url, resp.Status)
	}

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	klog.V(2).Infof("response is %v", string(responseBody))
	var response T
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return nil, fmt.Errorf("unmarshalling response body: %w", err)
	}
	return &response, nil
}

func doHTTPRequest(ctx context.Context, httpClient *http.Client, method string, url string, body any) (*http.Response, error) {
	requestBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshalling request body: %w", err)
	}
	req, err := http.NewRequest(method, url, bytes.NewBuffer(requestBody))
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")

	klog.V(2).Infof("sending request %v %v: %v", method, url, string(requestBody))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("making HTTP request: %w", err)
	}
	return resp, nil
}
