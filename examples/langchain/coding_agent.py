import asyncio
import sys
from typing import TypedDict, Literal, Optional
import torch
from transformers import AutoTokenizer, AutoModelForCausalLM

class AgentState(TypedDict):
    user_request: str
    generated_code: str
    execution_result: str
    error_message: Optional[str]
    iteration_count: int
    max_iterations: int
    status: Literal["planning", "coding", "executing", "fixing", "completed", "failed"]


class LocalCodeExecutor:
    
    @staticmethod
    async def execute(code: str) -> tuple[str, bool]:
        try:
            with open('/tmp/agent_code.py', 'w') as f:
                f.write(code)
            
            proc = await asyncio.create_subprocess_exec(
                'python', '/tmp/agent_code.py',
                stdout=asyncio.subprocess.PIPE,
                stderr=asyncio.subprocess.PIPE
            )
            
            try:
                stdout, stderr = await asyncio.wait_for(proc.communicate(), timeout=60.0)
                
                if proc.returncode == 0:
                    return stdout.decode(), True
                else:
                    return stderr.decode(), False
                    
            except asyncio.TimeoutError:
                proc.kill()
                return "Execution timeout (60s)", False
                
        except Exception as e:
            return f"Execution error: {str(e)}", False


class CodeGenerationLLM:
    
    def __init__(self, model_id: str = "Salesforce/codegen-350M-mono", hf_token: str = None):
        import os
        if not hf_token:
            hf_token = os.getenv("HF_TOKEN")
        
        if not hf_token:
            raise ValueError("HF_TOKEN required for model download")
        
        print(f"Loading model {model_id}... This will take 2-5 minutes on first run.")

        cache_dir = os.getenv("HF_HOME", "/models")
        
        self.tokenizer = AutoTokenizer.from_pretrained(
            model_id,
            token=hf_token,
            trust_remote_code=True,
            local_files_only=True,
            cache_dir=cache_dir
        )
        
        self.model = AutoModelForCausalLM.from_pretrained(
            model_id,
            token=hf_token,
            trust_remote_code=True,
            torch_dtype=torch.float16 if torch.cuda.is_available() else torch.float32,
            device_map="auto",
            low_cpu_mem_usage=True,
            local_files_only=True,
            cache_dir=cache_dir
        )
        
        if self.tokenizer.pad_token is None:
            self.tokenizer.pad_token = self.tokenizer.eos_token
        
        print(f"Model loaded successfully on device: {self.model.device}")
        print(f"Model memory footprint: ~{self.model.get_memory_footprint() / 1e9:.2f} GB")
    
    def generate_code_sync(self, task: str) -> str:
        prompt = f"""You are an expert programmer. Generate clean, executable code for the following task.
Your code must be completely self-contained with no external dependencies.
Include proper error handling and print informative output.
Output ONLY executable code, no markdown, no explanations, no backticks.

Task: {task}

Python code:"""
        
        inputs = self.tokenizer(prompt, return_tensors="pt", padding=True)
        inputs = {k: v.to(self.model.device) for k, v in inputs.items()}
        
        with torch.no_grad():
            outputs = self.model.generate(
                **inputs,
                max_new_tokens=256,
                temperature=0.1,
                do_sample=True,
                top_p=0.9,
                pad_token_id=self.tokenizer.pad_token_id,
                eos_token_id=self.tokenizer.eos_token_id,
                repetition_penalty=1.2
            )
        
        generated_text = self.tokenizer.decode(outputs[0], skip_special_tokens=True)
        
        code = generated_text[len(prompt):].strip()
        return self._clean_code(code)
    
    def fix_code_sync(self, task: str, code: str, error: str) -> str:
        prompt = f"""You are debugging code. The code failed with an error. Fix the code to resolve the error.
Output ONLY the corrected code, no markdown, no explanations, no backticks.

Original Task: {task}

Failed code:
{code}

Error:
{error}

Fixed Python code:"""
        
        inputs = self.tokenizer(prompt, return_tensors="pt", padding=True)
        inputs = {k: v.to(self.model.device) for k, v in inputs.items()}
        
        with torch.no_grad():
            outputs = self.model.generate(
                **inputs,
                max_new_tokens=256,
                temperature=0.1,
                do_sample=True,
                top_p=0.9,
                pad_token_id=self.tokenizer.pad_token_id,
                eos_token_id=self.tokenizer.eos_token_id,
                repetition_penalty=1.2
            )
        
        generated_text = self.tokenizer.decode(outputs[0], skip_special_tokens=True)
        
        code = generated_text[len(prompt):].strip()
        return self._clean_code(code)
    
    @staticmethod
    def _clean_code(code: str) -> str:
        code = code.strip()

        if code.startswith("```python"):
            code = code[9:].strip()
        elif code.startswith("```"):
            code = code[3:].strip()

        if code.endswith("```"):
            code = code[:-3].strip()

        lines = code.split('\n')
        cleaned_lines = []
        prev_line = None
        repeat_count = 0

        for line in lines:
            stripped = line.strip()

            if stripped.startswith(('Note:', 'Explanation:', 'This code', 'The code', 'Output:')):
                break
            
            if prev_line == stripped and len(stripped) < 3:
                repeat_count += 1
                if repeat_count > 2:  
                    break
            else:
                repeat_count = 0

            cleaned_lines.append(line)
            prev_line = stripped

        return '\n'.join(cleaned_lines).strip()


class CodingAgent:
    
    def __init__(self, llm: CodeGenerationLLM, executor: LocalCodeExecutor):
        self.llm = llm
        self.executor = executor
    
    async def generate_code_node(self, state: AgentState) -> AgentState:
        print("\nGenerating code...")
        
        loop = asyncio.get_event_loop()
        code = await loop.run_in_executor(None, self.llm.generate_code_sync, state["user_request"])
        
        print(f"Code generated ({len(code)} chars)")
        
        return {
            **state,
            "generated_code": code,
            "status": "executing"
        }
    
    async def execute_code_node(self, state: AgentState) -> AgentState:
        print("Executing code...")
        
        output, success = await self.executor.execute(state["generated_code"])
        
        if success:
            print("Execution successful")
            return {
                **state,
                "execution_result": output,
                "status": "completed",
                "error_message": None
            }
        else:
            print("Execution failed")
            return {
                **state,
                "execution_result": output,
                "error_message": output,
                "status": "fixing",
                "iteration_count": state["iteration_count"] + 1
            }
    
    async def fix_code_node(self, state: AgentState) -> AgentState:
        if state["iteration_count"] >= state["max_iterations"]:
            return {
                **state,
                "status": "failed",
                "execution_result": f"Max iterations reached ({state['max_iterations']})"
            }
        
        print(f"Fixing code (attempt {state['iteration_count']}/{state['max_iterations']})...")
        
        loop = asyncio.get_event_loop()
        fixed_code = await loop.run_in_executor(
            None, 
            self.llm.fix_code_sync,
            state["user_request"],
            state["generated_code"],
            state["error_message"]
        )
        
        print(f"Code fixed ({len(fixed_code)} chars)")
        
        return {
            **state,
            "generated_code": fixed_code,
            "status": "executing"
        }
    
    def should_continue(self, state: AgentState) -> Literal["execute", "fix", "end"]:
        if state["status"] == "executing":
            return "execute"
        elif state["status"] == "fixing":
            return "fix"
        else:
            return "end"


def create_graph(llm: CodeGenerationLLM, executor: LocalCodeExecutor):
    from langgraph.graph import StateGraph, END
    
    agent = CodingAgent(llm, executor)
    
    workflow = StateGraph(AgentState)
    workflow.add_node("generate", agent.generate_code_node)
    workflow.add_node("execute", agent.execute_code_node)
    workflow.add_node("fix", agent.fix_code_node)
    
    workflow.set_entry_point("generate")
    
    workflow.add_conditional_edges(
        "generate",
        agent.should_continue,
        {"execute": "execute", "end": END}
    )
    
    workflow.add_conditional_edges(
        "execute",
        agent.should_continue,
        {"fix": "fix", "end": END}
    )
    
    workflow.add_conditional_edges(
        "fix",
        agent.should_continue,
        {"execute": "execute", "end": END}
    )
    
    return workflow.compile()


async def run_agent(task: str, graph) -> dict:
    initial_state: AgentState = {
        "user_request": task,
        "generated_code": "",
        "execution_result": "",
        "error_message": None,
        "iteration_count": 0,
        "max_iterations": 3,
        "status": "planning"
    }
    
    final_state = await graph.ainvoke(initial_state)
    return final_state


async def interactive_chat():
    print("=" * 80)
    print("LangGraph Coding Agent (Local Transformers)")
    print("=" * 80)
    print("I can help you write and execute Python code.")
    print("Commands: 'exit', 'quit', or Ctrl+C to quit")
    print("=" * 80)
    
    print("\nInitializing LLM (this takes 2-5 minutes)...")
    llm = CodeGenerationLLM()
    executor = LocalCodeExecutor()
    graph = create_graph(llm, executor)
    print("Ready!\n")
    
    while True:
        try:
            print("\n" + "=" * 80)
            user_input = input("You: ").strip()
            
            if not user_input:
                continue
            
            if user_input.lower() in ['exit', 'quit']:
                print("\nGoodbye!")
                break
            
            print("")
            result = await run_agent(user_input, graph)
            
            print("\n" + "=" * 80)
            print("Generated Code:")
            print("-" * 80)
            print(result["generated_code"])
            print("\n" + "=" * 80)
            print("Execution Result:")
            print("-" * 80)
            print(result["execution_result"])
            print("=" * 80)
            
            if result["status"] == "failed":
                print(f"\nStatus: FAILED (tried {result['iteration_count']} times)")
            else:
                print(f"\nStatus: {result['status'].upper()}")
                if result["iteration_count"] > 0:
                    print(f"   (Fixed after {result['iteration_count']} attempts)")
            
        except KeyboardInterrupt:
            print("\n\nGoodbye!")
            break
        except Exception as e:
            print(f"\nError: {e}")
            import traceback
            traceback.print_exc()


def main():
    try:
        asyncio.run(interactive_chat())
    except KeyboardInterrupt:
        print("\n\nGoodbye!")


if __name__ == "__main__":
    main()